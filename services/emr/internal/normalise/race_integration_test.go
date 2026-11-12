//go:build integration

package normalise

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// asGateway runs q as core's role in f's hospital, retrying a deadlock or
// serialisation failure as core's route would be retried.
func (f *fixture) asGateway(t *testing.T, q string, args ...any) {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; ; attempt++ {
		err := func() error {
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, s := range []string{`SET LOCAL ROLE mhai_gateway`, `SELECT set_config('mhai.hospital_id', '` + f.hospital.String() + `', true)`} {
				if _, err := tx.Exec(ctx, s); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, q, args...); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		var pe *pgconn.PgError
		if err == nil {
			return
		}
		if errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "40001") && attempt < 20 {
			continue
		}
		t.Errorf("gateway %q: %v", q, err)
		return
	}
}

// handleUntilDone normalises ids, retrying a failed batch as a redelivery
// would.
func (f *fixture) handleUntilDone(t *testing.T, ids []uuid.UUID) {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		if f.n.Handle(context.Background(), f.hospital, ids) == nil {
			return
		}
	}
	t.Error("batch never normalised")
}

func (f *fixture) patientOf(t *testing.T, src string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT patient_id FROM emr.patient_links WHERE hospital_id = $1 AND source_patient_id = $2`,
		f.hospital, src).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Race review 1 (critical): an exclusion racing the normaliser on the same
// patient leaves nothing behind, whichever commits first: no canonical
// row, no payload naming the patient, and no name, MRN or birth date
// written back onto the excluded patient.
func TestExclusionRacesNormaliser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for round := 0; round < 8; round++ {
		src := fmt.Sprintf("p-race-%d", round)
		f.normalise(t, f.put(t, f.hospital, f.conn, patientRec(src, "MRN-R"+src, "1970-01-01", "female")))
		pid := f.patientOf(t, src)
		var recs []connector.RawRecord
		p2 := patientRec(src, "MRN-R"+src, "1970-01-01", "female")
		p2.SourceVersion = "2"
		recs = append(recs, p2)
		for i := 0; i < 40; i++ {
			recs = append(recs, obsRec(fmt.Sprintf("%s-o-%d", src, i), "1", src, nil))
		}
		ids := f.put(t, f.hospital, f.conn, recs...)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); f.handleUntilDone(t, ids) }()
		go func() {
			defer wg.Done()
			if round%2 == 1 {
				time.Sleep(time.Duration(round) * time.Millisecond)
			}
			f.asGateway(t, `SELECT * FROM emr_exclude_patient($1, 'objection')`, pid)
		}()
		wg.Wait()
		var obs, payloads int
		var name, mrn *string
		var birth *time.Time
		if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM emr.observations WHERE patient_id = $1),
			(SELECT count(*) FROM emr.raw_records WHERE hospital_id = $2 AND payload IS NOT NULL
			   AND (payload ->> 'patient' = $3 OR (resource_kind = 'patient' AND source_id = $3))),
			p.display_name, p.mrn, p.birth_date FROM emr.patients p WHERE p.id = $1`, pid, f.hospital, src).
			Scan(&obs, &payloads, &name, &mrn, &birth); err != nil {
			t.Fatal(err)
		}
		if obs != 0 || payloads != 0 || name != nil || mrn != nil || birth != nil {
			t.Fatalf("round %d: %d observations, %d payloads, name %v mrn %v birth %v remain", round, obs, payloads, name, mrn, birth)
		}
	}
}

// Race review 2: a merge racing the normaliser never leaves a record on
// the merged-away patient.
func TestMergeRacesNormaliser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c2 := uuid.New()
	f.addConn(t, f.hospital, c2, "Second record")
	for round := 0; round < 8; round++ {
		a, b := fmt.Sprintf("a-%d", round), fmt.Sprintf("b-%d", round)
		f.normalise(t, f.put(t, f.hospital, f.conn, patientRec(a, "MRN-M"+a, "1970-01-01", "male")))
		f.normalise(t, f.put(t, f.hospital, c2, patientRec(b, "MRN-M"+a, "1970-01-01", "male")))
		pa, pb := f.patientOf(t, a), f.patientOf(t, b)
		var match uuid.UUID
		if err := f.pool.QueryRow(ctx, `SELECT id FROM emr.possible_matches WHERE least(patient_a, patient_b) = least($1::uuid, $2::uuid)
			AND greatest(patient_a, patient_b) = greatest($1::uuid, $2::uuid)`, pa, pb).Scan(&match); err != nil {
			t.Fatalf("round %d: no proposal: %v", round, err)
		}
		keep := "a"
		var patientA uuid.UUID
		if err := f.pool.QueryRow(ctx, `SELECT patient_a FROM emr.possible_matches WHERE id = $1`, match).Scan(&patientA); err != nil {
			t.Fatal(err)
		}
		if patientA != pa {
			keep = "b"
		}
		var recs []connector.RawRecord
		for i := 0; i < 40; i++ {
			recs = append(recs, obsRec(fmt.Sprintf("%s-o-%d", b, i), "1", b, nil))
		}
		ids := f.put(t, f.hospital, c2, recs...)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); f.handleUntilDone(t, ids) }()
		go func() {
			defer wg.Done()
			if round%2 == 1 {
				time.Sleep(time.Duration(round) * time.Millisecond)
			}
			f.asGateway(t, `SELECT emr_merge_patients($1, $2)`, match, keep)
		}()
		wg.Wait()
		var onGone, onKept int
		if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM emr.observations WHERE patient_id = $1),
			(SELECT count(*) FROM emr.observations WHERE patient_id = $2)`, pb, pa).Scan(&onGone, &onKept); err != nil {
			t.Fatal(err)
		}
		if onGone != 0 || onKept != 40 {
			t.Fatalf("round %d: %d observations on the merged-away patient, %d on the kept one (want 0, 40)", round, onGone, onKept)
		}
	}
}

// Race review 3: the sink stores no payload for a patient already
// excluded, and a sink racing an exclusion leaves none behind either.
func TestSinkStoresNothingForExcludedPatient(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.normalise(t, f.put(t, f.hospital, f.conn, patientRec("p-x", "MRN-X", "1970-01-01", "male")))
	pid := f.patientOf(t, "p-x")
	f.asGateway(t, `SELECT * FROM emr_exclude_patient($1, 'objection')`, pid)
	ids := f.put(t, f.hospital, f.conn, obsRec("o-after", "1", "p-x", nil), patientRec("p-x", "MRN-X", "1970-01-01", "male"))
	if len(ids) != 0 {
		t.Errorf("announced %d records of an excluded patient", len(ids))
	}
	var payloads, rows int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE payload IS NOT NULL), count(*) FROM emr.raw_records
		WHERE hospital_id = $1 AND (source_id IN ('o-after', 'p-x'))`, f.hospital).Scan(&payloads, &rows); err != nil {
		t.Fatal(err)
	}
	if payloads != 0 || rows != 2 {
		t.Fatalf("payloads %d rows %d (want 0 payloads, 2 rows kept as rejected for the replay key)", payloads, rows)
	}
	// Racing.
	for round := 0; round < 8; round++ {
		src := fmt.Sprintf("p-s-%d", round)
		f.normalise(t, f.put(t, f.hospital, f.conn, patientRec(src, "MRN-S"+src, "1970-01-01", "male")))
		pid := f.patientOf(t, src)
		s := sink.New(f.pool, f.hospital, f.conn, "stub", 1, mrnNS, func(context.Context, uuid.UUID, []uuid.UUID) error { return nil })
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				if err := s.Put(ctx, obsRec(fmt.Sprintf("%s-o-%d", src, i), "1", src, nil)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(round) * time.Millisecond)
			f.asGateway(t, `SELECT * FROM emr_exclude_patient($1, 'objection')`, pid)
		}()
		wg.Wait()
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM emr.raw_records WHERE hospital_id = $1 AND payload ->> 'patient' = $2`,
			f.hospital, src).Scan(&payloads); err != nil {
			t.Fatal(err)
		}
		if payloads != 0 {
			t.Fatalf("round %d: %d payloads of the excluded patient remain", round, payloads)
		}
	}
}

// Race review 3: a batch dead-lettered while its patient was active keeps
// its payload; once the patient is excluded (here: marked directly, as a
// leftover no purge reached), the sweep clears it.
func TestDeadLetteredRecordOfExcludedPatientCleared(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.normalise(t, f.put(t, f.hospital, f.conn, patientRec("p-d", "MRN-D", "1970-01-01", "male")))
	pid := f.patientOf(t, "p-d")
	ids := f.put(t, f.hospital, f.conn, obsRec("o-dl", "1", "p-d", nil))
	broken := &Normaliser{Pool: brokenPool(t), Pub: f.pub}
	data, _ := proto.Marshal(&emrpb.Fetched{RawIds: []string{ids[0].String()}})
	for d := uint64(1); d <= MaxDeliver; d++ {
		broken.HandleMsg(ctx, &fakeMsg{subject: "emr.v1." + f.hospital.String() + ".fetched", data: data, deliveries: d})
	}
	if len(f.pub.on(".emr_normalise")) != 1 {
		t.Fatal("not dead-lettered")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE emr.patients SET status = 'excluded', excluded_at = now(), exclude_reason = 'other' WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	if s, _, payload := f.rawStatus(t, ids[0]); s != "pending" || !payload {
		t.Fatalf("before the sweep: %s payload %v", s, payload)
	}
	f.now = time.Now()
	if err := f.n.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if s, code, payload := f.rawStatus(t, ids[0]); s != "rejected" || code != "excluded" || payload {
		t.Fatalf("after the sweep: %s %s payload %v", s, code, payload)
	}
	var pp int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM emr.raw_records WHERE hospital_id = $1 AND resource_kind = 'patient'
		AND source_id = 'p-d' AND payload IS NOT NULL`, f.hospital).Scan(&pp); err != nil || pp != 0 {
		t.Fatalf("the patient's own record keeps its payload: %d %v", pp, err)
	}
}

// brokenPool is a pool that never reaches a database.
func brokenPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
