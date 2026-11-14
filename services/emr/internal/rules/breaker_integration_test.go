//go:build integration

package rules

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// seedFindings adds n critical, non-historical findings of the fixture's
// connection, created at each of times (n per time), on one observation.
func (f *fixture) seedFindings(t *testing.T, obs, patient uuid.UUID, n int, times ...time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, at := range times {
		if _, err := f.pool.Exec(ctx, `INSERT INTO emr.findings (hospital_id, patient_id, connection_id, rule_set_version,
			rule_set_sha256, rule_id, kind, category, severity, primary_observation_id, effective_at, received_at, status,
			shadow, preliminary, arrived_late, standardised, dedup_key, created_at)
			SELECT $1, $2, $3, '0-synthetic', decode(repeat('00', 32), 'hex'),
			       'seed', 'threshold', 'blood_chemistry', 'critical', $4, $5, $5, 'open', true, false, false, true,
			       'seed:' || gen_random_uuid(), $5
			FROM generate_series(1, $6)`, f.hospital, patient, f.conn, obs, at, n); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) oneObservation(t *testing.T) (obs, patient uuid.UUID) {
	t.Helper()
	ids := f.ingest(t, false, patientRec("p1"), kRec("k1", "1", "p1", "4.0", "final", time.Hour))
	if err := f.pool.QueryRow(context.Background(), `SELECT id, patient_id FROM emr.observations WHERE id = $1`, ids[0]).
		Scan(&obs, &patient); err != nil {
		t.Fatal(err)
	}
	return obs, patient
}

func (f *fixture) paused(t *testing.T) bool {
	t.Helper()
	var p bool
	if err := f.pool.QueryRow(context.Background(), `SELECT paused FROM emr_connections WHERE id = $1`, f.conn).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A32 (source): 200 criticals in 15 minutes against a baseline of 2 a
// window trips the breaker: the source is paused (the scheduler skips a
// paused source), a trip is recorded, and breaker_tripped is signalled by
// ids. The findings made before the trip are all announced.
func TestBreakerTripsAndPauses(t *testing.T) {
	f := newFixture(t)
	obs, patient := f.oneObservation(t)
	now := time.Now()
	var baseline []time.Time
	for i := 1; i <= 14*96; i++ {
		baseline = append(baseline, now.Add(-15*time.Minute-time.Duration(i)*15*time.Minute+time.Minute))
	}
	f.seedFindings(t, obs, patient, 2, baseline...)
	b := &Breaker{Pool: f.pool, Pub: f.pub, Multiple: 5, FloorCritical: 10, Now: func() time.Time { return now }}
	ctx := context.Background()
	// 10 in the window: under 5 x 2 and under the floor.
	f.seedFindings(t, obs, patient, 10, now.Add(-5*time.Minute))
	if tripped, err := b.Observe(ctx, f.hospital, f.conn); err != nil || tripped || f.paused(t) {
		t.Fatalf("10 in the window: tripped %v, %v", tripped, err)
	}
	f.seedFindings(t, obs, patient, 190, now.Add(-time.Minute))
	tripped, err := b.Observe(ctx, f.hospital, f.conn)
	if err != nil || !tripped || !f.paused(t) {
		t.Fatalf("200 in the window: tripped %v, %v, paused %v", tripped, err, f.paused(t))
	}
	var trips int
	var baselineSeen float64
	if err := f.pool.QueryRow(ctx, `SELECT count(*), max(baseline)::float8 FROM emr.breaker_trips WHERE connection_id = $1`, f.conn).
		Scan(&trips, &baselineSeen); err != nil || trips != 1 || baselineSeen != 2 {
		t.Fatalf("trips %d baseline %v %v", trips, baselineSeen, err)
	}
	sig := f.pub.on(".signal")
	if len(sig) != 1 {
		t.Fatalf("signals %+v", sig)
	}
	var s emrpb.Signal
	if err := proto.Unmarshal(sig[0].data, &s); err != nil || s.Kind != "breaker_tripped" || s.ConnectionId != f.conn.String() {
		t.Fatalf("signal %v %v", &s, err)
	}
	// Already paused: no second trip, no second signal.
	if tripped, err := b.Observe(ctx, f.hospital, f.conn); err != nil || tripped || len(f.pub.on(".signal")) != 1 {
		t.Fatalf("second observe: %v %v", tripped, err)
	}
	// The scheduler's view: a paused source is not due.
	var due int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM emr_connections WHERE id = $1 AND enabled AND NOT paused`, f.conn).
		Scan(&due); err != nil || due != 0 {
		t.Fatalf("paused source still due: %d %v", due, err)
	}
}

// Both the multiple and the floor: a quiet source (baseline 0) trips only
// past the floor; a busy one only past the multiple.
func TestBreakerNeedsBothMultipleAndFloor(t *testing.T) {
	f := newFixture(t)
	obs, patient := f.oneObservation(t)
	now := time.Now()
	ctx := context.Background()
	b := &Breaker{Pool: f.pool, Pub: f.pub, Multiple: 5, FloorCritical: 10, Now: func() time.Time { return now }}
	f.seedFindings(t, obs, patient, 10, now.Add(-time.Minute))
	if tripped, err := b.Observe(ctx, f.hospital, f.conn); err != nil || tripped {
		t.Fatalf("10 against 0, floor 10: %v %v", tripped, err)
	}
	// A busy source: 3 a window for 14 days; 12 now is over the floor but
	// under 5 x 3.
	var baseline []time.Time
	for i := 1; i <= 14*96; i++ {
		baseline = append(baseline, now.Add(-15*time.Minute-time.Duration(i)*15*time.Minute+time.Minute))
	}
	f.seedFindings(t, obs, patient, 3, baseline...)
	f.seedFindings(t, obs, patient, 2, now.Add(-time.Minute))
	if tripped, err := b.Observe(ctx, f.hospital, f.conn); err != nil || tripped {
		t.Fatalf("12 against 3: %v %v", tripped, err)
	}
	f.seedFindings(t, obs, patient, 4, now.Add(-time.Minute))
	if tripped, err := b.Observe(ctx, f.hospital, f.conn); err != nil || !tripped {
		t.Fatalf("16 against 3: %v %v", tripped, err)
	}
	// Another hospital's connection is never paused by this hospital's
	// breaker (the definer is bound to the caller's hospital).
	other := uuid.New()
	var changed bool
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `SET LOCAL ROLE mhai_emr`)
	_, _ = tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, other.String())
	if err := tx.QueryRow(ctx, `SELECT emr_pause_connection($1)`, f.conn).Scan(&changed); err != nil || changed {
		t.Fatalf("another hospital paused it: %v %v", changed, err)
	}
}

// The consumer runs the breaker after announcing, and only while the rule
// set alerts for the hospital: in shadow no source is paused. Active, every
// finding made before the trip is announced, and the source is paused.
func TestConsumerBreakerAnnouncesThenPauses(t *testing.T) {
	f := newFixture(t)
	f.shadowOnly(t)
	ctx := context.Background()
	f.eng.Breaker = &Breaker{Pool: f.pool, Pub: f.pub, Multiple: 1, FloorCritical: 2}
	recs := func(prefix string) []connector.RawRecord {
		out := []connector.RawRecord{patientRec("p1")}
		for i := range 5 {
			out = append(out, kRec(prefix+string(rune('a'+i)), "1", "p1", "6.8", "final", time.Hour))
		}
		return out
	}
	if err := f.eng.Handle(ctx, f.hospital, f.ingest(t, false, recs("s-")...)); err != nil {
		t.Fatal(err)
	}
	if f.paused(t) || len(f.pub.on(".signal")) != 0 {
		t.Fatal("a source was paused while the rule set is in shadow")
	}
	sha := f.eng.RuleSet.SHA256()
	f.eng.Active = f.eng.RuleSet.Version()
	f.activate(t, f.eng.RuleSet.Version(), sha[:])
	if err := f.eng.Handle(ctx, f.hospital, f.ingest(t, false, recs("a-")...)); err != nil {
		t.Fatal(err)
	}
	// The five shadow findings are never announced; the five live ones are.
	if n := len(f.pub.on(".finding")); n != 5 {
		t.Fatalf("%d announced, want 5", n)
	}
	if !f.paused(t) || len(f.pub.on(".signal")) != 1 {
		t.Fatalf("paused %v signals %d", f.paused(t), len(f.pub.on(".signal")))
	}
}

// A source with less than 14 days of findings is held to a baseline of 4
// a window, so 15 criticals (over the floor of 10) don't pause it; 21 do.
func TestBreakerYoungSourceBaselineFloor(t *testing.T) {
	f := newFixture(t)
	obs, patient := f.oneObservation(t)
	now := time.Now()
	ctx := context.Background()
	b := &Breaker{Pool: f.pool, Pub: f.pub, Multiple: 5, FloorCritical: 10, Now: func() time.Time { return now }}
	f.seedFindings(t, obs, patient, 15, now.Add(-time.Minute))
	if tripped, err := b.Observe(ctx, f.hospital, f.conn); err != nil || tripped {
		t.Fatalf("15 on a young source: %v %v", tripped, err)
	}
	f.seedFindings(t, obs, patient, 6, now.Add(-time.Minute))
	if tripped, err := b.Observe(ctx, f.hospital, f.conn); err != nil || !tripped {
		t.Fatalf("21 on a young source: %v %v", tripped, err)
	}
}
