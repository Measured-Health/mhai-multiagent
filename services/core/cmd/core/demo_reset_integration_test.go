//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/feedback"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

func resetEnv(extra map[string]string) func(string) string {
	m := map[string]string{demoSeedEnv: "on", "APP_ENV": "demo", "MFA_ENCRYPTION_KEY": testDemoKey}
	maps.Copy(m, extra)
	return demoEnv(m)
}

func runReset(t *testing.T, url string, env func(string) string) string {
	t.Helper()
	var out bytes.Buffer
	if err := demoCmd(context.Background(), []string{"reset"}, url, env, &out); err != nil {
		t.Fatalf("demo reset: %v", err)
	}
	for _, a := range DemoAccounts {
		if strings.Contains(out.String(), a.Email) {
			t.Fatalf("output names an account: %s", out.String())
		}
	}
	if strings.Contains(out.String(), DemoPassword) || strings.Contains(out.String(), DemoTOTPSecret) || strings.Contains(out.String(), "visitor") {
		t.Fatal("output holds a credential or visitor text")
	}
	return out.String()
}

// visitorQuery is a consultation a visitor asked, in status; completed ones
// carry an answer and an evaluation, as the worker writes them.
func visitorQuery(t *testing.T, super *pgxpool.Pool, hid, by uuid.UUID, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var started, completed any
	if status != "queued" {
		started, completed = at.Add(time.Second), at.Add(9*time.Second)
	}
	if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, patient_data_json, clinical_question, urgency, category,
		status, progress, started_at, completed_at, created_by, created_at, attempts, published_at, deadline_at, error)
		VALUES ($1, $2, '{"age": 40}', 'A visitor question, synthetic', 'routine', 'General Practice', $3::text, 1, $4, $5, $6, $7, 1, $7,
		        $7::timestamptz + interval '10 minutes', CASE WHEN $3::text = 'failed' THEN 'deadline_exceeded' END)`,
		id, hid, status, started, completed, by, at); err != nil {
		t.Fatalf("query %s: %v", status, err)
	}
	if status == "completed" {
		if _, err := super.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, response_text, trust_score, hga_decision,
			verified, degraded, failed_agents, masked_question, masked_patient_json) VALUES ($1, $2, $3, 'Sample answer, synthetic', 0.9, 'PASS',
			true, false, '{}', 'A visitor question, synthetic', '{"age": 40}')`, uuid.New(), hid, id); err != nil {
			t.Fatal(err)
		}
		if _, err := super.Exec(ctx, `INSERT INTO hga_evaluations (hospital_id, query_id, target_agent, trust, decision)
			VALUES ($1, $2, 'CCA', 0.9, 'PASS')`, hid, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func correct(t *testing.T, super *pgxpool.Pool, hid, by, qid uuid.UUID) uuid.UUID {
	t.Helper()
	d, r := "A visitor diagnosis", "A visitor reason, synthetic"
	sub, err := (feedback.Store{Pool: super}).Submit(context.Background(), hid, by, "", qid,
		feedback.FeedbackIn{QueryID: qid.String(), Action: feedbackCorrect, CorrectedDiagnosis: &d, Reason: &r})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return sub.CorrectionID
}

func approve(t *testing.T, super *pgxpool.Pool, hid, by, cid uuid.UUID) {
	t.Helper()
	if _, err := (feedback.Store{Pool: super}).Decide(context.Background(), hid, by, cid, "", feedback.Decision{Kind: "approve"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

func finishJobs(t *testing.T, super *pgxpool.Pool, cid uuid.UUID) {
	t.Helper()
	if _, err := super.Exec(context.Background(), `UPDATE jobs SET status = 'completed', completed_at = now() WHERE subject_id = $1`, cid); err != nil {
		t.Fatal(err)
	}
}

func session(t *testing.T, super *pgxpool.Pool, uid uuid.UUID) uuid.UUID {
	t.Helper()
	sid := uuid.New()
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, true, '{pwd,otp}')`, sid, uid); err != nil {
		t.Fatal(err)
	}
	return sid
}

// hospitalRows counts every row of the tables the reset deletes from or
// updates, for one hospital.
func hospitalRows(t *testing.T, super *pgxpool.Pool, hid uuid.UUID) map[string]int {
	t.Helper()
	m := map[string]int{}
	for _, tbl := range []string{"clinical_queries", "clinical_decisions", "hga_evaluations", "feedback_events", "feedback_buffer", "jobs"} {
		m[tbl] = scalar(t, super, `SELECT count(*) FROM `+tbl+` WHERE hospital_id = $1`, hid)
	}
	m["active users"] = scalar(t, super, `SELECT count(*) FROM users WHERE hospital_id = $1 AND is_active`, hid)
	m["live sessions"] = scalar(t, super, `SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE u.hospital_id = $1 AND s.revoked_at IS NULL`, hid)
	m["trusted devices"] = scalar(t, super, `SELECT count(*) FROM trusted_devices d JOIN users u ON u.id = d.user_id WHERE u.hospital_id = $1`, hid)
	return m
}

func TestDemoResetRestoresAccountsAndRemovesVisitorData(t *testing.T) {
	ctx := context.Background()
	pg, super := testutil.MigratedPostgres(t)
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	buckets, err := bus.OpenBuckets(ctx, js, 1)
	if err != nil {
		t.Fatal(err)
	}
	env := resetEnv(map[string]string{"NATS_URL": n.GatewayURL})

	runDemo(t, pg.URL)
	var hid uuid.UUID
	if err := super.QueryRow(ctx, `SELECT id FROM hospitals WHERE short_code = 'DEM'`).Scan(&hid); err != nil {
		t.Fatal(err)
	}
	reyes, _, _ := demoUser(t, super, "clinician")
	director, _, _ := demoUser(t, super, "director")
	newID, _, _ := demoUser(t, super, "newAccount")

	// Another demo hospital beside DEM (the only kind the guards allow), with
	// its own consultations, feedback, people and sessions: never touched.
	liv := analysisHospital(t, super, "LIV")
	var livA, livB uuid.UUID
	for _, u := range []struct {
		id    *uuid.UUID
		email string
	}{{&livA, "a.liv@liv.example"}, {&livB, "b.liv@liv.example"}} {
		if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, $2, 'Synthetic') RETURNING id`,
			liv, u.email).Scan(u.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician'), ($2, 'clinician')`, livA, livB); err != nil {
		t.Fatal(err)
	}
	livQ := visitorQuery(t, super, liv, livA, "completed")
	visitorQuery(t, super, liv, livA, "failed")
	livC := correct(t, super, liv, livA, livQ)
	approve(t, super, liv, livB, livC)
	finishJobs(t, super, livC)
	session(t, super, livA)
	livBefore := hospitalRows(t, super, liv)

	// What visitors did in DEM.
	other, _ := password.Hash("visitor-" + uuid.NewString())
	if _, err := super.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, reyes, other); err != nil {
		t.Fatal(err)
	}
	reyesSession := session(t, super, reyes)
	if _, err := super.Exec(ctx, `UPDATE users SET must_change_password = false, terms_version = '2026-10', terms_accepted_at = now()
		WHERE id = $1`, newID); err != nil {
		t.Fatal(err)
	}
	sealer, _ := auth.NewTOTPSealer([]byte(testDemoKey))
	own, _ := auth.RandomTOTPSecret()
	enc, _ := sealer.Seal(newID, own)
	if _, err := super.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled, backup_code_hashes) VALUES ($1, $2, true, '{}')`,
		newID, enc); err != nil {
		t.Fatal(err)
	}
	done := visitorQuery(t, super, hid, reyes, "completed")
	visitorQuery(t, super, hid, reyes, "failed")
	cancelled := visitorQuery(t, super, hid, reyes, "cancelled")
	if _, err := super.Exec(ctx, `INSERT INTO query_cancellations (query_id, hospital_id, cancelled_by) VALUES ($1, $2, $3)`,
		cancelled, hid, director); err != nil {
		t.Fatal(err)
	}
	inFlight := visitorQuery(t, super, hid, reyes, "queued")
	undrained := visitorQuery(t, super, hid, reyes, "completed")
	if _, err := super.Exec(ctx, `INSERT INTO intelligence_audit_outbox (hospital_id, query_id, action) VALUES ($1, $2, 'query.completed')`,
		hid, undrained); err != nil {
		t.Fatal(err)
	}
	// A visitor's correction, approved, learnt from (the buffer row) and done.
	vc := correct(t, super, hid, reyes, done)
	approve(t, super, hid, director, vc)
	finishJobs(t, super, vc)
	if _, err := super.Exec(ctx, `INSERT INTO feedback_buffer (hospital_id, correction_id, system_decision) VALUES ($1, $2, 'PASS')`,
		hid, vc); err != nil {
		t.Fatal(err)
	}
	// A visitor approved the waiting sample correction; its job finished.
	var pending uuid.UUID
	if err := super.QueryRow(ctx, `SELECT id FROM feedback_events WHERE hospital_id = $1 AND status = 'pending_review'`, hid).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	approve(t, super, hid, director, pending)
	finishJobs(t, super, pending)
	// A visitor's correction whose learning job is still running: kept.
	busyQ := visitorQuery(t, super, hid, reyes, "completed")
	busy := correct(t, super, hid, reyes, busyQ)
	approve(t, super, hid, director, busy)
	// An account a visitor (the sample admin) added, signed in.
	var added uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, display_name) VALUES ($1, 'added.by.visitor@demo-hospital.example', 'Added')
		RETURNING id`, hid).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, added); err != nil {
		t.Fatal(err)
	}
	addedSession := session(t, super, added)

	// Sign-in counters: the clinician's per-login ceiling and two-step
	// failures (visitors locked it), and another login's ceiling.
	counters := &auth.Service{MFAKey: []byte(testDemoKey)}
	ceiling, _ := counters.CeilingKey(demoAccountByKey("clinician").Email)
	mfaFail, _ := counters.MFAFailKey(reyes.String())
	otherCeiling, _ := counters.CeilingKey("someone@liv.example")
	for _, p := range []struct {
		kv interface {
			Put(context.Context, string, []byte) (uint64, error)
		}
		key string
	}{{buckets.LoginCeiling, ceiling}, {buckets.MFAAttempts, mfaFail}, {buckets.LoginCeiling, otherCeiling}} {
		if _, err := p.kv.Put(ctx, p.key, []byte("20")); err != nil {
			t.Fatal(err)
		}
	}

	out := runReset(t, pg.URL, env)
	if !strings.Contains(out, "demo reset hospital "+hid.String()) {
		t.Fatalf("output: %s", out)
	}

	// Accounts back to their published state, everyone signed out.
	if _, h, _ := demoUser(t, super, "clinician"); func() bool { ok, _ := password.Verify(DemoPassword, h); return !ok }() {
		t.Fatal("changed password not restored")
	}
	if _, _, must := demoUser(t, super, "newAccount"); !must {
		t.Fatal("new account's first steps not restored")
	}
	if n := scalar(t, super, `SELECT count(*) FROM user_mfa WHERE user_id = $1`, newID); n != 0 {
		t.Fatal("two-step a visitor enrolled on the new account kept")
	}
	for _, sid := range []uuid.UUID{reyesSession, addedSession} {
		if n := scalar(t, super, `SELECT count(*) FROM sessions WHERE id = $1 AND revoked_at IS NOT NULL`, sid); n != 1 {
			t.Fatalf("session %s not revoked", sid)
		}
	}
	if n := scalar(t, super, `SELECT count(*) FROM users WHERE id = $1 AND NOT is_active`, added); n != 1 {
		t.Fatal("added account not deactivated")
	}
	// Counters: the sample account's cleared, another login's kept.
	if _, err := buckets.LoginCeiling.Get(ctx, ceiling); !bus.IsNotFound(err) {
		t.Fatalf("ceiling not cleared: %v", err)
	}
	if _, err := buckets.MFAAttempts.Get(ctx, mfaFail); !bus.IsNotFound(err) {
		t.Fatalf("two-step counter not cleared: %v", err)
	}
	if _, err := buckets.LoginCeiling.Get(ctx, otherCeiling); err != nil {
		t.Fatalf("another login's counter touched: %v", err)
	}

	// Visitor data gone; samples, in-flight and undrained kept.
	seeded := 0
	for _, c := range demoConsultations {
		seeded += scalar(t, super, `SELECT count(*) FROM clinical_queries q JOIN clinical_decisions d ON d.query_id = q.id
			WHERE q.id = $1`, uuid.NewSHA1(demoNS, []byte(hid.String()+"/consultation/"+c.key)))
	}
	if seeded != len(demoConsultations) {
		t.Fatalf("%d seeded consultations left", seeded)
	}
	want := map[string]int{
		// 5 seeded, the queued one, the undrained one, the one with a busy correction.
		"clinical_queries": 8,
		"feedback_events":  3, // the two samples (the waiting one made again) and the busy one
	}
	for tbl, v := range want {
		if got := scalar(t, super, `SELECT count(*) FROM `+tbl+` WHERE hospital_id = $1`, hid); got != v {
			t.Errorf("%s: %d rows, want %d", tbl, got, v)
		}
	}
	for _, id := range []uuid.UUID{inFlight, undrained, busyQ} {
		if n := scalar(t, super, `SELECT count(*) FROM clinical_queries WHERE id = $1`, id); n != 1 {
			t.Errorf("query %s removed", id)
		}
	}
	if n := scalar(t, super, `SELECT count(*) FROM clinical_queries WHERE id = ANY($1)`, []uuid.UUID{done, cancelled}); n != 0 {
		t.Error("visitor consultations kept")
	}
	if n := scalar(t, super, `SELECT count(*) FROM query_cancellations WHERE hospital_id = $1`, hid); n != 0 {
		t.Error("cancellation record kept")
	}
	if n := scalar(t, super, `SELECT count(*) FROM feedback_events WHERE id = ANY($1)`, []uuid.UUID{vc, pending}); n != 0 {
		t.Error("visitor corrections kept")
	}
	if n := scalar(t, super, `SELECT count(*) FROM feedback_buffer WHERE hospital_id = $1`, hid); n != 0 {
		t.Error("visitor learning facts kept")
	}
	if n := scalar(t, super, `SELECT count(*) FROM feedback_events WHERE hospital_id = $1 AND status = 'pending_review'
		AND query_id = $2`, hid, uuid.NewSHA1(demoNS, []byte(hid.String()+"/consultation/bp"))); n != 1 {
		t.Error("the waiting sample correction not made again")
	}
	if n := scalar(t, super, `SELECT count(*) FROM feedback_events WHERE id = $1 AND status = 'approved'`, busy); n != 1 {
		t.Error("a correction with a running job removed")
	}

	// Another hospital's rows: exactly as before.
	if got := hospitalRows(t, super, liv); !maps.Equal(got, livBefore) {
		t.Fatalf("LIV changed: %v, was %v", got, livBefore)
	}

	// Audited, counts only; the chain holds.
	if n := scalar(t, super, `SELECT count(*) FROM audit_log WHERE action = 'demo.reset' AND hospital_id = $1
		AND (payload->>'consultations')::int = 3 AND (payload->>'feedback')::int = 2 AND (payload->>'deactivated')::int = 1
		AND (payload->>'counters')::int = 2 AND (payload->>'in_flight')::int = 3`, hid); n != 1 {
		var p string
		_ = super.QueryRow(ctx, `SELECT coalesce(string_agg(payload::text, ' '), '') FROM audit_log WHERE action = 'demo.reset'`).Scan(&p)
		t.Fatalf("demo.reset audit rows: %d (%s)", n, p)
	}
	if ok, broken, err := audit.Verify(ctx, super); err != nil || !ok {
		t.Fatalf("audit chain: ok=%v broken=%d err=%v", ok, broken, err)
	}

	// Again: idempotent. Nothing more removed or audited, samples the same.
	before := hospitalRows(t, super, hid)
	runReset(t, pg.URL, env)
	after := hospitalRows(t, super, hid)
	if !maps.Equal(before, after) {
		t.Fatalf("second reset changed DEM: %v, was %v", after, before)
	}
	if n := scalar(t, super, `SELECT count(*) FROM audit_log WHERE action = 'demo.reset'`); n != 1 {
		t.Fatalf("an idle reset was audited (%d rows)", n)
	}

	// An operator's kill switch stays off across a reset.
	if err := analysisSwitchOff(ctx, "--global", pg.URL, io.Discard); err != nil {
		t.Fatalf("switch off --global: %v", err)
	}
	runReset(t, pg.URL, env)
	if n := scalar(t, super, `SELECT count(*) FROM analysis_switch WHERE hospital_id IS NULL AND position = 'on'`); n != 0 {
		t.Fatal("the reset raised the global analysis switch an operator lowered")
	}
}

// The reset runs every guard of the seed before it writes anything.
func TestDemoResetRefusals(t *testing.T) {
	ctx := context.Background()
	prepared := func(t *testing.T) (*testutil.PG, *pgxpool.Pool, uuid.UUID, uuid.UUID) {
		pg, super := testutil.MigratedPostgres(t)
		runDemo(t, pg.URL)
		var hid uuid.UUID
		if err := super.QueryRow(ctx, `SELECT id FROM hospitals WHERE short_code = 'DEM'`).Scan(&hid); err != nil {
			t.Fatal(err)
		}
		reyes, _, _ := demoUser(t, super, "clinician")
		return pg, super, hid, visitorQuery(t, super, hid, reyes, "completed")
	}
	untouched := func(t *testing.T, super *pgxpool.Pool, q uuid.UUID) {
		t.Helper()
		if n := scalar(t, super, `SELECT count(*) FROM clinical_queries WHERE id = $1`, q); n != 1 {
			t.Fatal("visitor data removed by a refused reset")
		}
		if n := scalar(t, super, `SELECT count(*) FROM audit_log WHERE action = 'demo.reset'`); n != 0 {
			t.Fatal("a refused reset was audited")
		}
	}

	t.Run("a non-demo hospital", func(t *testing.T) {
		pg, super, _, q := prepared(t)
		if _, err := super.Exec(ctx, `INSERT INTO hospitals (name, short_code, status, time_zone) VALUES ('Synthetic Real', 'RLH', 'active', 'UTC')`); err != nil {
			t.Fatal(err)
		}
		if err := demoCmd(ctx, []string{"reset"}, pg.URL, resetEnv(nil), &bytes.Buffer{}); !errors.Is(err, errDemoReal) {
			t.Fatalf("%v, want errDemoReal", err)
		}
		untouched(t, super, q)
	})
	t.Run("without the demo flags", func(t *testing.T) {
		pg, super, _, q := prepared(t)
		for _, env := range []map[string]string{
			{demoSeedEnv: "", "APP_ENV": "demo"},
			{demoSeedEnv: "on", "APP_ENV": "development"},
		} {
			if err := demoCmd(ctx, []string{"reset"}, pg.URL, resetEnv(env), &bytes.Buffer{}); !errors.Is(err, errDemoOff) {
				t.Fatalf("%v: %v, want errDemoOff", env, err)
			}
		}
		untouched(t, super, q)
	})
	t.Run("not the owner", func(t *testing.T) {
		pg, super, _, q := prepared(t)
		_, provURL, err := testutil.ProvisionLogin(ctx, pg, super)
		if err != nil {
			t.Fatal(err)
		}
		if err := demoCmd(ctx, []string{"reset"}, provURL, resetEnv(nil), &bytes.Buffer{}); !errors.Is(err, errDemoOwner) {
			t.Fatalf("%v", err)
		}
		untouched(t, super, q)
	})
	t.Run("NATS configured and unreachable", func(t *testing.T) {
		pg, super, _, q := prepared(t)
		err := demoCmd(ctx, []string{"reset"}, pg.URL, resetEnv(map[string]string{"NATS_URL": "nats://127.0.0.1:1"}), &bytes.Buffer{})
		if !errors.Is(err, errDemoNATS) {
			t.Fatalf("%v, want errDemoNATS", err)
		}
		untouched(t, super, q)
	})
	t.Run("without NATS it still resets the database", func(t *testing.T) {
		pg, super, _, q := prepared(t)
		out := runReset(t, pg.URL, resetEnv(nil))
		if !strings.Contains(out, "left to expire") {
			t.Fatalf("output: %s", out)
		}
		if n := scalar(t, super, `SELECT count(*) FROM clinical_queries WHERE id = $1`, q); n != 0 {
			t.Fatal("visitor consultation kept")
		}
	})
}

// A reset beside a seed or another reset (a deploy during the schedule)
// serialises on the seed's advisory lock.
func TestDemoResetConcurrent(t *testing.T) {
	pg, super := testutil.MigratedPostgres(t)
	runDemo(t, pg.URL)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := "reset"
			if i%2 == 1 {
				cmd = "seed"
			}
			errs[i] = demoCmd(context.Background(), []string{cmd}, pg.URL, resetEnv(nil), &bytes.Buffer{})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := scalar(t, super, `SELECT count(*) FROM clinical_queries`); n != 5 {
		t.Fatalf("%d consultations", n)
	}
	if n := scalar(t, super, `SELECT count(*) FROM feedback_events`); n != 2 {
		t.Fatalf("%d corrections", n)
	}
}

// NATS up but core hasn't made its buckets yet: nothing is counted, so the
// reset clears nothing there and goes on.
func TestDemoResetBeforeCoreMadeBuckets(t *testing.T) {
	ctx := context.Background()
	pg, _ := testutil.MigratedPostgres(t)
	n, stop, err := testutil.NewNATS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	runDemo(t, pg.URL)
	out := runReset(t, pg.URL, resetEnv(map[string]string{"NATS_URL": n.GatewayURL}))
	if !strings.Contains(out, "counters 0 cleared") {
		t.Fatalf("output: %s", out)
	}
}
