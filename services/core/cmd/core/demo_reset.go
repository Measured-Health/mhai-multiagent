package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
)

// The scheduled reset of the public demo ("core demo reset"; deploy/README.md,
// "Public demo: the scheduled reset"). Visitors share the sample accounts, so
// every few hours the reset takes out what they made in DEM and the seed puts
// the accounts and samples back. It runs inside demoSeeder.run, after the
// advisory lock and both demoGuards checks: it can only ever touch DEM, in a
// database that holds demo hospitals only. Every statement names DEM's id;
// the owner bypasses row-level security, so the id is the only wall and it
// is in every WHERE.

var errDemoNATS = errors.New("could not clear the sample accounts' sign-in counters in NATS; nothing was reset")

// demoReset is what "core demo reset" adds to the seed.
type demoReset struct {
	// counters derives the counter key names from MFA_ENCRYPTION_KEY, as
	// core does (only CeilingKey and MFAFailKey are used: no bucket, no
	// database).
	counters          *auth.Service
	natsURL, natsSeed string
	natsTLS           bus.TLS
}

// demoResetCounts are the reset's counts, for the audit row and the output.
type demoResetCounts struct {
	consultations, feedback, jobs, deactivated, inFlight, counters int
}

func (c demoResetCounts) any() bool {
	return c.consultations+c.feedback+c.jobs+c.deactivated+c.counters > 0
}

// resetVisitors clears the sample accounts' counters, then removes the
// visitors' data in one transaction, audited in it (demo.reset, counts only).
func (s *demoSeeder) resetVisitors(ctx context.Context) error {
	var c demoResetCounts
	var err error
	if c.counters, err = s.clearCounters(ctx); err != nil {
		return err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := s.removeVisitorData(ctx, tx, &c); err != nil {
			return err
		}
		if !c.any() {
			return nil
		}
		if err := audit.Write(ctx, tx, audit.Event{HospitalID: s.hid.String(), Action: "demo.reset", Entity: "hospital",
			EntityID: s.hid.String(), Payload: map[string]any{"via": "cli", "consultations": c.consultations,
				"feedback": c.feedback, "jobs": c.jobs, "deactivated": c.deactivated, "in_flight": c.inFlight,
				"counters": c.counters}}); err != nil {
			return demoErr(err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "demo reset hospital %s: consultations %d removed (%d in flight kept); feedback %d removed; jobs %d removed; added accounts %d deactivated; counters %d cleared\n",
		s.hid, c.consultations, c.inFlight, c.feedback, c.jobs, c.deactivated, c.counters)
	return nil
}

// demoEmails are the sample accounts' emails, normalised as stored.
func demoEmails() []string {
	out := make([]string, 0, len(DemoAccounts))
	for _, a := range DemoAccounts {
		out = append(out, account.NormaliseEmail(a.Email))
	}
	return out
}

// removeVisitorData deletes, in DEM only, everything visitors made: the
// feedback that isn't a pristine seeded sample (with its learning facts and
// finished approve_correction jobs), then the consultations that aren't
// seeded samples (with their answers and evaluations), and deactivates the
// accounts visitors added. Kept for the next run: anything still in flight
// (a question or job queued or running) and anything whose audit facts
// the outbox hasn't drained yet (deleting those would lose audit facts).
func (s *demoSeeder) removeVisitorData(ctx context.Context, tx pgx.Tx, c *demoResetCounts) error {
	hid := s.hid
	// Lock DEM's rows first: a decision, a claim or a retry waits for this
	// transaction, and every statement below (READ COMMITTED, a fresh
	// snapshot each) sees what committed before the locks.
	if _, err := execAll(ctx, tx, []any{hid},
		`SELECT 1 FROM clinical_queries WHERE hospital_id = $1 FOR UPDATE`,
		`SELECT 1 FROM feedback_events WHERE hospital_id = $1 FOR UPDATE`,
		`SELECT 1 FROM jobs WHERE hospital_id = $1 FOR UPDATE`); err != nil {
		return err
	}

	var seeded []uuid.UUID
	for _, dc := range demoConsultations {
		seeded = append(seeded, s.consultationID(dc.key))
	}
	var sQuery []uuid.UUID
	var sDiag, sReason, sStatus []string
	for _, dc := range demoCorrections {
		sQuery = append(sQuery, s.consultationID(dc.key))
		sDiag, sReason = append(sDiag, dc.diagnosis), append(sReason, dc.reason)
		st := "pending_review"
		if dc.approve {
			st = "approved"
		}
		sStatus = append(sStatus, st)
	}
	clinician := account.NormaliseEmail(demoAccountByKey("clinician").Email)
	director := account.NormaliseEmail(demoAccountByKey("director").Email)

	// Feedback to remove: not a pristine sample correction (the sample
	// clinician's, on its sample consultation, with the sample text, waiting
	// or approved by the sample director as documented), and with no job in
	// flight and no undrained audit fact.
	doomed, err := queryIDs(ctx, tx, `SELECT f.id FROM feedback_events f
		WHERE f.hospital_id = $1
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest($2::uuid[], $3::text[], $4::text[], $5::text[]) AS s(qid, diagnosis, reason, status)
		      WHERE f.query_id = s.qid AND f.action = 'correct' AND f.status = s.status
		        AND f.corrected_diagnosis = s.diagnosis AND f.reason = s.reason
		        AND f.submitted_by = (SELECT id FROM users WHERE email = $6 AND hospital_id = $1)
		        AND ((s.status = 'pending_review' AND f.decided_by IS NULL)
		          OR (s.status = 'approved' AND f.decided_by = (SELECT id FROM users WHERE email = $7 AND hospital_id = $1))))
		  AND NOT EXISTS (
		      SELECT 1 FROM jobs j WHERE j.hospital_id = $1 AND j.subject_id = f.id
		      AND (j.status NOT IN ('completed', 'failed', 'cancelled')
		           OR EXISTS (SELECT 1 FROM intelligence_audit_outbox o WHERE o.hospital_id = $1 AND o.job_id = j.id)))`,
		hid, sQuery, sDiag, sReason, sStatus, clinician, director)
	if err != nil {
		return err
	}
	if len(doomed) > 0 {
		args := []any{hid, doomed}
		if c.jobs, err = execAll(ctx, tx, args,
			`DELETE FROM feedback_buffer WHERE hospital_id = $1 AND correction_id = ANY($2)`,
			`DELETE FROM jobs WHERE hospital_id = $1 AND type = 'approve_correction' AND subject_id = ANY($2)`); err != nil {
			return err
		}
		if c.feedback, err = execAll(ctx, tx, args, `DELETE FROM feedback_events WHERE hospital_id = $1 AND id = ANY($2)`); err != nil {
			return err
		}
	}

	// Consultations to remove: not a seeded sample, finished, no undrained
	// audit fact, no feedback left on it.
	gone, err := queryIDs(ctx, tx, `SELECT q.id FROM clinical_queries q
		WHERE q.hospital_id = $1 AND q.id <> ALL($2::uuid[])
		  AND q.status IN ('completed', 'failed', 'cancelled')
		  AND NOT EXISTS (SELECT 1 FROM intelligence_audit_outbox o WHERE o.hospital_id = $1 AND o.query_id = q.id)
		  AND NOT EXISTS (SELECT 1 FROM feedback_events f WHERE f.hospital_id = $1 AND f.query_id = q.id)`, hid, seeded)
	if err != nil {
		return err
	}
	if len(gone) > 0 {
		// query_cancellations go with the queries (ON DELETE CASCADE).
		if c.consultations, err = execAll(ctx, tx, []any{hid, gone},
			`DELETE FROM hga_evaluations WHERE hospital_id = $1 AND query_id = ANY($2)`,
			`DELETE FROM clinical_decisions WHERE hospital_id = $1 AND query_id = ANY($2)`,
			`DELETE FROM clinical_queries WHERE hospital_id = $1 AND id = ANY($2)`); err != nil {
			return err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM clinical_queries WHERE hospital_id = $1 AND id <> ALL($2::uuid[])`,
		hid, seeded).Scan(&c.inFlight); err != nil {
		return demoErr(err)
	}

	// Accounts a visitor (a sample hospital admin) added to DEM: deactivated
	// (their sessions end with them, and a question of theirs still in flight
	// is cancelled by 00009's trigger). Their rows stay: the audit chain and
	// their history name them.
	ids, err := queryIDs(ctx, tx, `UPDATE users SET is_active = false
		WHERE hospital_id = $1 AND is_active AND email <> ALL($2::text[]) RETURNING id`, hid, demoEmails())
	if err != nil {
		return err
	}
	c.deactivated = len(ids)
	if len(ids) > 0 {
		_, err = execAll(ctx, tx, []any{ids},
			`UPDATE sessions SET revoked_at = now() WHERE user_id = ANY($1) AND revoked_at IS NULL`,
			`DELETE FROM trusted_devices WHERE user_id = ANY($1)`)
	}
	return err
}

// queryIDs returns the uuid each row of sql names.
func queryIDs(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err == nil {
		var ids []uuid.UUID
		if ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err == nil {
			return ids, nil
		}
	}
	return nil, demoErr(err)
}

// execAll runs each statement in tx with the same args, in order, and
// returns the rows the last one affected.
func execAll(ctx context.Context, tx pgx.Tx, args []any, stmts ...string) (int, error) {
	n := 0
	for _, q := range stmts {
		tag, err := tx.Exec(ctx, q, args...)
		if err != nil {
			return 0, demoErr(err)
		}
		n = int(tag.RowsAffected())
	}
	return n, nil
}

// clearCounters deletes the sample accounts' per-login sign-in ceiling (a
// visitor's wrong passwords lock that login for everyone for an hour) and
// their two-step failure counters, in core's own KV buckets, by the key
// names core derives (auth.CeilingKey, auth.MFAFailKey). Only those keys:
// another account's counters are never touched, and the per-(login, IP)
// counters, which lock only the visitor's own address, expire after 15
// minutes on their own (their names need the address). It runs before
// anything is written: if NATS is configured and fails, nothing is reset.
func (s *demoSeeder) clearCounters(ctx context.Context) (int, error) {
	r := s.reset
	if r.natsURL == "" {
		fmt.Fprintln(s.out, "demo reset: NATS_URL is not set; the sign-in counters are left to expire")
		return 0, nil
	}
	nc, js, err := bus.Connect(ctx, r.natsURL, r.natsSeed, bus.GatewayInboxPrefix, r.natsTLS)
	if err != nil {
		return 0, errDemoNATS
	}
	defer nc.Close()

	ids, err := queryIDs(ctx, s.pool, `SELECT id FROM users WHERE hospital_id = $1 AND email = ANY($2::text[])`, s.hid, demoEmails())
	if err != nil {
		return 0, err
	}
	// The key names; any error (no usable MFA key) is checked once.
	var ceilingKeys, mfaKeys []string
	var keyErr error
	for _, a := range DemoAccounts {
		k, err := r.counters.CeilingKey(a.Email)
		ceilingKeys, keyErr = append(ceilingKeys, k), errors.Join(keyErr, err)
	}
	for _, id := range ids {
		k, err := r.counters.MFAFailKey(id.String())
		mfaKeys, keyErr = append(mfaKeys, k), errors.Join(keyErr, err)
	}
	if keyErr != nil {
		return 0, errDemoNATS
	}

	n := 0
	for bucket, keys := range map[string][]string{bus.LoginCeilingBucket: ceilingKeys, bus.MFAAttemptsBucket: mfaKeys} {
		kv, err := js.KeyValue(ctx, bucket)
		if errors.Is(err, jetstream.ErrBucketNotFound) {
			continue // core hasn't made it yet: nothing is counted
		}
		if err != nil {
			return 0, errDemoNATS
		}
		for _, k := range keys {
			if _, err := kv.Get(ctx, k); bus.IsNotFound(err) {
				continue
			} else if err != nil {
				return 0, errDemoNATS
			}
			// Delete, as core's own refund does: the next failure starts
			// the count again from one.
			if err := kv.Delete(ctx, k); err != nil {
				return 0, errDemoNATS
			}
			n++
		}
	}
	return n, nil
}
