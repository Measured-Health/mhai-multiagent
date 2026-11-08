package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/synth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

const analysisUsage = `usage:
  core analysis seed-demo <hospital id or short code> [--seed N] [--months 1-24]
  core analysis switch off <hospital id or short code> | --global

seed-demo is for the synthetic demo hospital only (analysis plan, departure
5). It sets the global and the hospital's analysis switches on with the
references DEMO-SYNTHETIC, refusing if either carries a real sign-off
(SO-...), writes a seeded synthetic cohort through mhai_analysis_seed, and
audits analysis.switch_raised. A hospital under a demo switch never ingests
a real consultation. Raising a switch with a real sign-off is the platform's
two-person flow (Plan 11b), never this command.

switch off lowers a switch at once and audits analysis.switch_lowered.

Both connect with DATABASE_URL as the provisioning login (mhai_provision).
seed-demo prints the hospital id and the subject count only.`

var (
	errAnalysisArgs  = fmt.Errorf("%s", analysisUsage)
	errRealSwitch    = errors.New("an analysis switch carries a real sign-off; the CLI never changes it (use the platform)")
	errAnalysisWrite = errors.New("could not write the analysis switch or seed; nothing was changed")
)

const demoRef = "DEMO-SYNTHETIC"

// analysisDemoSubjects is the demo cohort's size.
const analysisDemoSubjects = 600

// analysisCmd is "core analysis ...". clock is when the seed acts (nil:
// the database's now()); every argument is checked before connecting.
func analysisCmd(ctx context.Context, args []string, url string, out io.Writer,
	clock func(context.Context) (time.Time, error)) error {
	switch {
	case len(args) >= 2 && args[0] == "seed-demo":
		return analysisSeedDemo(ctx, args[1:], url, out, clock)
	case len(args) == 3 && args[0] == "switch" && args[1] == "off":
		return analysisSwitchOff(ctx, args[2], url, out)
	}
	return errAnalysisArgs
}

func analysisSeedDemo(ctx context.Context, args []string, url string, out io.Writer,
	clock func(context.Context) (time.Time, error)) error {
	ref := args[0]
	if strings.HasPrefix(ref, "-") || account.ValidateHospitalRef(ref) != nil {
		return errAnalysisArgs
	}
	seed, months := int64(1), 24
	rest := args[1:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			return errAnalysisArgs
		}
		v := rest[1]
		switch rest[0] {
		case "--seed":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return errAnalysisArgs
			}
			seed = n
		case "--months":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 24 {
				return errAnalysisArgs
			}
			months = n
		default:
			return errAnalysisArgs
		}
		rest = rest[2:]
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	var subjects int
	hospital, err := account.ProvisionOnHospital(ctx, pool, ref, func(tx pgx.Tx, hospital uuid.UUID) error {
		var now time.Time
		var err error
		if clock != nil {
			now, err = clock(ctx)
		} else {
			err = tx.QueryRow(ctx, `SELECT now()`).Scan(&now)
		}
		if err != nil {
			return errAnalysisWrite
		}
		var real bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM analysis_switch
			WHERE (hospital_id IS NULL OR hospital_id = $1)
			  AND (signoff_clinical LIKE 'SO-%' OR signoff_privacy LIKE 'SO-%' OR signoff_legal LIKE 'SO-%'))`, hospital).Scan(&real); err != nil {
			return errAnalysisWrite
		}
		if real {
			return errRealSwitch
		}
		raised := map[string]bool{}
		for scope, h := range map[string]*uuid.UUID{"global": nil, "hospital": &hospital} {
			var was string
			err := tx.QueryRow(ctx, `SELECT position FROM analysis_switch WHERE hospital_id IS NOT DISTINCT FROM $1`, h).Scan(&was)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return errAnalysisWrite
			}
			if _, err := tx.Exec(ctx, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
				VALUES ($1, 'on', $2, $2, $2)
				ON CONFLICT (hospital_id) DO UPDATE SET position = 'on', signoff_clinical = EXCLUDED.signoff_clinical,
				    signoff_privacy = EXCLUDED.signoff_privacy, signoff_legal = EXCLUDED.signoff_legal, changed_by = NULL`, h, demoRef); err != nil {
				return errAnalysisWrite
			}
			raised[scope] = was != "on"
		}
		start := now.UTC().AddDate(0, -months, -2)
		cohort := synth.Cohort(seed, synth.Options{Subjects: analysisDemoSubjects, Months: months, SmallBias: 0.3, Start: start})
		body, err := cohort.SeedJSON()
		if err != nil {
			return errAnalysisWrite
		}
		if err := tx.QueryRow(ctx, `SELECT mhai_analysis_seed($1, $2, $3::jsonb)`, hospital, now, string(body)).Scan(&subjects); err != nil {
			return errAnalysisWrite
		}
		// The audit last, in this transaction (the chain lock until commit).
		for _, scope := range []string{"global", "hospital"} {
			if !raised[scope] {
				continue
			}
			p := map[string]any{"via": "cli", "refs": "demo"}
			if scope == "hospital" {
				p["hospital_id"] = hospital.String()
			}
			if err := audit.Write(ctx, tx, audit.Event{Action: "analysis.switch_raised", Payload: p}); err != nil {
				return errAnalysisWrite
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "analysis demo %s %d\n", hospital, subjects)
	return nil
}

func analysisSwitchOff(ctx context.Context, target, url string, out io.Writer) error {
	global := target == "--global"
	if !global && (strings.HasPrefix(target, "-") || account.ValidateHospitalRef(target) != nil) {
		return errAnalysisArgs
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	lower := func(tx pgx.Tx, h *uuid.UUID) (bool, error) {
		tag, err := tx.Exec(ctx, `UPDATE analysis_switch SET position = 'off'
			WHERE hospital_id IS NOT DISTINCT FROM $1 AND position = 'on'`, h)
		if err != nil {
			return false, errAnalysisWrite
		}
		if tag.RowsAffected() == 0 {
			return false, nil
		}
		p := map[string]any{"via": "cli"}
		if h != nil {
			p["hospital_id"] = h.String()
		}
		if err := audit.Write(ctx, tx, audit.Event{Action: "analysis.switch_lowered", Payload: p}); err != nil {
			return false, errAnalysisWrite
		}
		return true, nil
	}
	var changed bool
	name := "global"
	if global {
		err = provisionTx(ctx, pool, func(tx pgx.Tx) error {
			changed, err = lower(tx, nil)
			return err
		})
	} else {
		var h uuid.UUID
		h, err = account.ProvisionOnHospital(ctx, pool, target, func(tx pgx.Tx, h uuid.UUID) error {
			var err error
			changed, err = lower(tx, &h)
			return err
		})
		name = h.String()
	}
	if err != nil {
		return err
	}
	if !changed {
		fmt.Fprintf(out, "analysis %s already off; nothing changed\n", name)
		return nil
	}
	fmt.Fprintf(out, "analysis %s off\n", name)
	return nil
}

// provisionTx runs fn as mhai_provision in one READ COMMITTED transaction.
func provisionTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return errors.New("begin failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_provision`); err != nil {
		return account.ErrNotProvisioner
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("commit failed")
	}
	return nil
}
