package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

const emrUsage = `usage:
  core emr exclude --hospital <id or short code> (--patient <id> | --mrn-stdin) --reason objection|hospital_policy|other
  core emr remove-hospital-data <hospital id or short code>
  core hospitals legal-hold place|lift <hospital id or short code> --reference REF

emr exclude is the operator's fallback for a hospital's written exclusion
request (EMR design §10.6): the same database function as the hospital
admin's route, audited the same way (emr.patient_excluded, alert.withdrawn,
and emr.exclusion_purged or, under a legal hold, emr.exclusion_purge_held;
via cli, no actor). It connects with DATABASE_URL as the operator's EMR
login, a member of mhai_emr_admin only (EMR_ADMIN_DB_USER at core migrate
up). --mrn-stdin reads the MRN from the first line of stdin, so it is never
an argument (shell history, the process list). It prints ids only.

emr remove-hospital-data is the hook of the hospital removal flow (plan
departure 11): it deletes the hospital's EMR rows and alert tables, audited
emr.hospital_data_removed with counts. The database refuses unless the
hospital is removal_scheduled, has no open legal hold, and every
connection's credentials were wiped first. It connects as the provisioning
login (mhai_provision).

hospitals legal-hold place|lift (plan departure 10) places or lifts a legal
hold on the hospital, audited legal_hold.placed or legal_hold.lifted (via
cli). While any hold is open nothing of the hospital's EMR data is deleted:
no exclusion purge, no retention, no removal. --reference is a document
reference such as LH-2026-0042, never a name. It connects as the
provisioning login.`

var (
	errEMRArgs         = fmt.Errorf("%s", emrUsage)
	errNotEMRAdmin     = errors.New("this login can't act as mhai_emr_admin (connect as the operator's EMR login)")
	errNoPatient       = errors.New("no active patient of the hospital matches")
	errAmbiguousMRN    = errors.New("the MRN names more than one active patient: use --patient")
	errNoMRN           = errors.New("no MRN on stdin (one line, at most 64 characters)")
	errExcludeWrite    = errors.New("could not exclude the patient; nothing was changed")
	errExcludeBusy     = errors.New("the record feed is busy with this patient; nothing was changed, try again")
	errRemovalRefused  = errors.New("refused: the hospital is not removal_scheduled; nothing was deleted")
	errRemovalHold     = errors.New("refused: the hospital is under a legal hold; nothing was deleted")
	errRemovalCreds    = errors.New("refused: wipe every connection's credentials first; nothing was deleted")
	errRemovalWrite    = errors.New("could not remove the hospital's EMR data; nothing was deleted")
	errLegalHoldArgs   = errors.New("usage: core hospitals legal-hold place|lift <hospital id or short code> --reference REF")
	errLegalHoldRef    = errors.New("--reference must be a document reference: 2 to 8 capitals, a year and 2 to 8 digits (LH-2026-0042)")
	errLegalHoldExists = errors.New("an open legal hold with that reference already exists; nothing changed")
	errLegalHoldNone   = errors.New("no open legal hold with that reference; nothing changed")
	errLegalHoldWrite  = errors.New("could not change the legal hold; nothing was changed")
)

// legalHoldRef is legal_holds.reference's CHECK (00032).
var legalHoldRef = regexp.MustCompile(`^[A-Z]{2,8}-[0-9]{4}-[0-9]{2,8}$`)

// emrCmd is "core emr ...". Errors and output never hold an MRN or a name.
func emrCmd(ctx context.Context, args []string, url string, stdin io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errEMRArgs
	}
	switch args[0] {
	case "exclude":
		return emrExclude(ctx, args[1:], url, stdin, out)
	case "remove-hospital-data":
		return emrRemoveHospitalData(ctx, args[1:], url, out)
	}
	return errEMRArgs
}

// excludeArgs is a checked "emr exclude" command line.
type excludeArgs struct {
	hospital, reason string
	patient          uuid.UUID
	mrnStdin         bool
}

func parseExclude(args []string) (excludeArgs, error) {
	fs := newFlags("emr exclude")
	var a excludeArgs
	var patient string
	fs.StringVar(&a.hospital, "hospital", "", "")
	fs.StringVar(&patient, "patient", "", "")
	fs.BoolVar(&a.mrnStdin, "mrn-stdin", false, "")
	fs.StringVar(&a.reason, "reason", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || a.hospital == "" || (patient == "") == !a.mrnStdin ||
		!slices.Contains(alerts.ExclusionReasons, a.reason) {
		return a, errEMRArgs
	}
	if err := account.ValidateHospitalRef(a.hospital); err != nil {
		return a, err
	}
	if patient != "" {
		id, err := uuid.Parse(patient)
		if err != nil || id.String() != patient || id == uuid.Nil {
			return a, errEMRArgs
		}
		a.patient = id
	}
	return a, nil
}

// readMRN takes the first line of r, minus its ending: 1 to 64 characters.
func readMRN(r io.Reader) (string, error) {
	line, err := bufio.NewReader(io.LimitReader(r, 66)).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", errNoMRN
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" || len(line) > 64 {
		return "", errNoMRN
	}
	return line, nil
}

// emrExclude is "emr exclude" (alerts.ExcludePatient as mhai_emr_admin).
func emrExclude(ctx context.Context, args []string, url string, stdin io.Reader, out io.Writer) error {
	a, err := parseExclude(args)
	if err != nil {
		return err
	}
	mrn := ""
	if a.mrnStdin {
		if mrn, err = readMRN(stdin); err != nil {
			return err
		}
	}
	cond, arg, err := account.HospitalWhere(a.hospital)
	if err != nil {
		return err
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	var x alerts.ExclusionResult
	var pid uuid.UUID
	for attempt := 0; ; attempt++ {
		err = func() error {
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				return errExcludeWrite
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_emr_admin`); err != nil {
				return errNotEMRAdmin
			}
			var hid uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM hospitals WHERE `+cond, arg).Scan(&hid); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return account.ErrNoSuchHospital
				}
				return errExcludeWrite
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('mhai.hospital_id', $1, true)`, hid.String()); err != nil {
				return errExcludeWrite
			}
			var rows pgx.Rows
			// The login never reads an MRN column: a definer answers at most
			// two ids of the hospital set.
			if a.mrnStdin {
				rows, err = tx.Query(ctx, `SELECT id FROM emr_patients_by_mrn($1) id`, mrn)
			} else {
				rows, err = tx.Query(ctx, `SELECT $1::uuid WHERE emr_patient_in_hospital($1)`, a.patient)
			}
			if err != nil {
				return errExcludeWrite
			}
			found, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			switch {
			case err != nil:
				return errExcludeWrite
			case len(found) == 0:
				return errNoPatient
			case len(found) > 1:
				return errAmbiguousMRN
			}
			pid = found[0]
			if x, err = alerts.ExcludePatient(ctx, tx, alerts.AuditBy{HospitalID: hid.String(), Via: "cli"}, pid, a.reason); err != nil {
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
			return nil
		}()
		var pe *pgconn.PgError
		if errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "40001") && attempt < 2 {
			time.Sleep(time.Duration(attempt+1) * 50 * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		var pe *pgconn.PgError
		switch {
		case errors.As(err, &pe) && (pe.Code == "40P01" || pe.Code == "40001"):
			return errExcludeBusy
		case errors.As(err, &pe) && pe.Code == "P0002":
			return errNoPatient
		}
		for _, known := range []error{errNotEMRAdmin, account.ErrNoSuchHospital, errNoPatient, errAmbiguousMRN} {
			if errors.Is(err, known) {
				return err
			}
		}
		return errExcludeWrite
	}
	state := "purged"
	if x.Held {
		state = "held (legal hold; purged once lifted)"
	}
	fmt.Fprintf(out, "excluded patient %s: purge request %s %s, %d alerts withdrawn\n", pid, x.RequestID, state, len(x.Withdrawn))
	return nil
}

// emrRemoveHospitalData is "emr remove-hospital-data <ref>"
// (emr_remove_hospital_data as mhai_provision, audited
// emr.hospital_data_removed).
func emrRemoveHospitalData(ctx context.Context, args []string, url string, out io.Writer) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errEMRArgs
	}
	if err := account.ValidateHospitalRef(args[0]); err != nil {
		return err
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	var counts map[string]any
	id, err := account.ProvisionOnHospital(ctx, pool, args[0], func(tx pgx.Tx, hospital uuid.UUID) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT emr_remove_hospital_data($1)`, hospital).Scan(&raw); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "55000" {
				// The function's own fixed messages, never data.
				switch {
				case strings.Contains(pe.Message, "legal hold"):
					return errRemovalHold
				case strings.Contains(pe.Message, "credentials"):
					return errRemovalCreds
				}
				return errRemovalRefused
			}
			return errRemovalWrite
		}
		if err := json.Unmarshal(raw, &counts); err != nil {
			return errRemovalWrite
		}
		payload := map[string]any{"via": "cli"}
		for _, k := range audit.HospitalDataRemovedKeys {
			n, ok := counts[k].(float64)
			if !ok {
				return errRemovalWrite
			}
			payload[k] = int64(n)
		}
		if err := audit.Write(ctx, tx, audit.Event{HospitalID: hospital.String(), Action: "emr.hospital_data_removed",
			Entity: "hospital", EntityID: hospital.String(), Payload: payload}); err != nil {
			return errRemovalWrite
		}
		return nil
	})
	if err != nil {
		return err
	}
	var total int64
	for _, v := range counts {
		if n, ok := v.(float64); ok {
			total += int64(n)
		}
	}
	fmt.Fprintf(out, "removed the EMR data of hospital %s: %d rows\n", id, total)
	return nil
}

// hospitalsLegalHold is "hospitals legal-hold place|lift <ref> --reference
// R" (legal_holds as mhai_provision, audited legal_hold.placed or
// legal_hold.lifted). The reference is checked before connecting and
// never printed.
func hospitalsLegalHold(ctx context.Context, args []string, url string, out io.Writer) error {
	if len(args) < 2 || (args[0] != "place" && args[0] != "lift") || strings.HasPrefix(args[1], "-") {
		return errLegalHoldArgs
	}
	verb, ref := args[0], args[1]
	fs := newFlags("hospitals legal-hold")
	var reference string
	fs.StringVar(&reference, "reference", "", "")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 || reference == "" {
		return errLegalHoldArgs
	}
	if !legalHoldRef.MatchString(reference) {
		return errLegalHoldRef
	}
	if err := account.ValidateHospitalRef(ref); err != nil {
		return err
	}
	pool, err := openOwner(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	var hold uuid.UUID
	id, err := account.ProvisionOnHospital(ctx, pool, ref, func(tx pgx.Tx, hospital uuid.UUID) error {
		action := "legal_hold.placed"
		if verb == "place" {
			if err := tx.QueryRow(ctx, `INSERT INTO legal_holds (hospital_id, reference) VALUES ($1, $2) RETURNING id`,
				hospital, reference).Scan(&hold); err != nil {
				var pe *pgconn.PgError
				if errors.As(err, &pe) && pe.Code == "23505" {
					return errLegalHoldExists
				}
				return errLegalHoldWrite
			}
		} else {
			action = "legal_hold.lifted"
			if err := tx.QueryRow(ctx, `UPDATE legal_holds SET lifted_at = now()
				WHERE hospital_id = $1 AND reference = $2 AND lifted_at IS NULL RETURNING id`, hospital, reference).Scan(&hold); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errLegalHoldNone
				}
				return errLegalHoldWrite
			}
		}
		if err := audit.Write(ctx, tx, audit.Event{HospitalID: hospital.String(), Action: action, Entity: "legal_hold",
			EntityID: hold.String(), Payload: map[string]any{"hold_id": hold.String(), "via": "cli"}}); err != nil {
			return errLegalHoldWrite
		}
		return nil
	})
	if err != nil {
		return err
	}
	verbed := "placed on"
	if verb == "lift" {
		verbed = "lifted from"
	}
	fmt.Fprintf(out, "legal hold %s %s hospital %s\n", hold, verbed, id)
	return nil
}
