//go:build integration

package analysis

import (
	"bytes"
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
)

// Analysis PR 10 (Task 28, departure 87): an excluded patient's subject is
// forgotten by the nightly run once the EMR purge is done: its facts in
// unfrozen blocks and its reference go, and October's stored body and raw
// cells (frozen) are unchanged. Every value is synthetic; the clock is
// pinned (12 November 2026).
func TestExclusionLeavesFrozenBodyUnchanged(t *testing.T) {
	super, app := migrated(t)
	ctx := context.Background()
	w := newWorld(t, super, app)
	w.cohort(t, map[string]int{"female": 30, "male": 20})
	keys := testKeys(t)

	// One cohort subject is a real-looking EMR patient: its reference is
	// sealed with the keys, and it has a fact in an unfrozen block too.
	patient := uuid.New()
	exec(t, super, `INSERT INTO emr.patients (id, hospital_id, display_name, mrn, birth_date, sex)
		VALUES ($1, $2, 'Synthetic Orsolya Vantreek', 'SYN-AN10-0042', '1983-06-09', 'female')`, patient, w.h)
	s := w.subject(t, "female", w.src, w.conds[0], "2026-10-02")
	enc, err := keys.SealRef(w.h, patient.String())
	if err != nil {
		t.Fatal(err)
	}
	exec(t, super, `UPDATE analysis_subjects SET ref_hmac = $2, ref_enc = $3 WHERE subject_id = $1`, s, keys.RefHMAC(w.h, patient.String()), enc)
	exec(t, super, `INSERT INTO analysis_facts (hospital_id, subject_id, day, kind, condition_id, age_band, sex, period_late)
		VALUES ($1, $2, '2026-11-10', 'diagnosis', $3, '40-64', 'female', false)`, w.h, s, w.conds[0])
	w.freeze(t)
	e := w.engine()
	p := w.period(t, e, october)
	first, err := e.Body(as(w.h), p, disclose.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	var rawBefore disclose.Raw
	if err := gateway(t, app, w.h, func(tx pgx.Tx) error {
		var err error
		rawBefore, err = readCells(ctx, tx, p, female())
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// The exclusion, purged (no hold), before the run's clock.
	exec(t, super, `UPDATE emr.patients SET status = 'excluded', excluded_at = $2, exclude_reason = 'objection' WHERE id = $1`,
		patient, clockAt.Add(-time.Hour))
	exec(t, super, `INSERT INTO emr.purge_requests (hospital_id, patient_id, requested_at, due_at, state, done_at)
		VALUES ($1, $2, $3, $3, 'done', $3)`, w.h, patient, clockAt.Add(-time.Hour))
	n := NewNightly(app, keys, nil)
	n.Clock = fixedClock(clockAt)
	if ran, err := n.Tick(ctx); err != nil || len(ran) != 1 || ran[0] != w.h {
		t.Fatalf("ran %v (%v)", ran, err)
	}

	if c := count(t, super, `SELECT count(*) FROM analysis_subjects WHERE subject_id = $1 AND ref_enc IS NULL AND ref_hmac <> $2`,
		s, keys.RefHMAC(w.h, patient.String())); c != 1 {
		t.Error("the excluded subject keeps its reference")
	}
	if c := count(t, super, `SELECT count(*) FROM analysis_facts WHERE subject_id = $1 AND day = '2026-11-10'`, s); c != 0 {
		t.Error("the unfrozen fact is left")
	}
	if c := count(t, super, `SELECT count(*) FROM analysis_facts WHERE subject_id = $1 AND day = '2026-10-02'`, s); c != 1 {
		t.Error("the frozen fact went")
	}
	again, err := w.engine().Body(as(w.h), p, disclose.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range disclose.Views {
		if !bytes.Equal(first.Views[string(v)], again.Views[string(v)]) {
			t.Errorf("%s changed after the exclusion", v)
		}
	}
	var rawAfter disclose.Raw
	if err := gateway(t, app, w.h, func(tx pgx.Tx) error {
		var err error
		rawAfter, err = readCells(ctx, tx, p, female())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rawBefore, rawAfter) {
		t.Error("October's raw cells changed after the exclusion")
	}
}
