// Package index is the per-hospital patient index (EMR design §10; plan
// Task 20). Within one connection the source patient id decides, always: a
// new source id is a new MHAI patient. Across connections, and for a
// source's own merge, the index only proposes: it writes an
// emr.possible_matches row and returns a signal for core, and a hospital
// admin confirms (core's emr_merge_patients). Nothing is ever matched by
// name, and nothing merges here.
//
// Every query runs in the caller's transaction, as mhai_emr under the
// connection's hospital (RLS); every join on patients names hospital_id.
package index

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/signals"
)

// PatientFields are a Patient resource's fields, already cleaned. A
// pointer field is unknown when nil.
type PatientFields struct {
	DisplayName, MRN string
	BirthDate        *time.Time
	Sex              string // female, male, other or unknown
}

// Patient is a resolved MHAI patient.
type Patient struct {
	ID     uuid.UUID
	Status string // active, merged_into, excluded
}

// ErrSourceID: an empty or over-long source patient id.
var ErrSourceID = errors.New("index: no usable source patient id")

// NormaliseMRN is the form an MRN is stored and compared in: spaces
// removed, upper case. Nothing else is changed.
func NormaliseMRN(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// Resolve is the MHAI patient of (conn, sourcePatientID), created with p's
// fields if new. With p, an active patient's fields are updated (an
// excluded patient's never are: its data stays purged).
//
// Locks (race review): concurrent calls for one source id are serialised
// by a transaction-level advisory lock, which emr_exclude_patient takes
// too; the patient row is held FOR SHARE until the caller commits, so an
// exclusion or a merge (FOR UPDATE) waits for this transaction, or this
// one waits for it and then sees the patient excluded or merged. A merged
// patient is followed to the one it was merged into, under the same lock.
func Resolve(ctx context.Context, tx pgx.Tx, conn connector.Config, sourcePatientID string, p *PatientFields, rawID *uuid.UUID) (Patient, error) {
	if sourcePatientID == "" || len(sourcePatientID) > 256 {
		return Patient{}, ErrSourceID
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, LockKey(conn.ConnectionID, sourcePatientID)); err != nil {
		return Patient{}, err
	}
	out, found, err := Lookup(ctx, tx, conn.HospitalID, conn.ConnectionID, sourcePatientID)
	if err != nil {
		return Patient{}, err
	}
	if !found {
		f := fieldsOrEmpty(p)
		if err := tx.QueryRow(ctx, `INSERT INTO emr.patients (hospital_id, display_name, mrn, birth_date, sex, raw_id)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, status`, conn.HospitalID, nullable(f.DisplayName), nullable(f.MRN),
			f.BirthDate, sex(f.Sex), rawID).Scan(&out.ID, &out.Status); err != nil {
			return Patient{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO emr.patient_links (hospital_id, connection_id, source_patient_id, patient_id)
			VALUES ($1, $2, $3, $4)`, conn.HospitalID, conn.ConnectionID, sourcePatientID, out.ID); err != nil {
			return Patient{}, err
		}
		return out, nil
	}
	if p != nil && out.Status == "active" {
		f := fieldsOrEmpty(p)
		if _, err := tx.Exec(ctx, `UPDATE emr.patients SET display_name = $3, mrn = $4, birth_date = $5, sex = $6,
			raw_id = $7, updated_at = now() WHERE hospital_id = $1 AND id = $2 AND status = 'active'`, conn.HospitalID, out.ID,
			nullable(f.DisplayName), nullable(f.MRN), f.BirthDate, sex(f.Sex), rawID); err != nil {
			return Patient{}, err
		}
	}
	return out, nil
}

// LockKey is the advisory lock key of one source patient id; core's
// emr_exclude_patient builds the same text.
func LockKey(connection uuid.UUID, sourcePatientID string) string {
	return "emr-link:" + connection.String() + ":" + sourcePatientID
}

// maxMergeDepth bounds following merged_into (links move with a merge, so
// one step is the usual case).
const maxMergeDepth = 16

// Lookup is the patient (conn, sourcePatientID) links to, if any, held FOR
// SHARE until the caller commits; a merged patient is followed to the one
// it was merged into. It creates nothing.
func Lookup(ctx context.Context, tx pgx.Tx, hospital, connection uuid.UUID, sourcePatientID string) (Patient, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT l.patient_id FROM emr.patient_links l
		WHERE l.hospital_id = $1 AND l.connection_id = $2 AND l.source_patient_id = $3`,
		hospital, connection, sourcePatientID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Patient{}, false, nil
	}
	if err != nil {
		return Patient{}, false, err
	}
	for range maxMergeDepth {
		var out Patient
		var into *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT p.id, p.status, p.merged_into FROM emr.patients p
			WHERE p.hospital_id = $1 AND p.id = $2 FOR SHARE`, hospital, id).Scan(&out.ID, &out.Status, &into); err != nil {
			return Patient{}, false, err
		}
		if out.Status != "merged_into" || into == nil {
			return out, true, nil
		}
		id = *into
	}
	return Patient{}, false, ErrMergeChain
}

// ErrMergeChain: merged_into deeper than maxMergeDepth (fixed text).
var ErrMergeChain = errors.New("index: merge chain too deep")

func fieldsOrEmpty(p *PatientFields) PatientFields {
	if p == nil {
		return PatientFields{Sex: "unknown"}
	}
	f := *p
	f.MRN = NormaliseMRN(f.MRN)
	return f
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sex(s string) string {
	switch s {
	case "female", "male", "other":
		return s
	}
	return "unknown"
}

// Propose writes the possible matches of patient (just resolved through
// conn) with active patients of other connections of the hospital that
// share its MRN, when both connections declare the same MRN namespace
// (settings.mrn_namespace). Equal MRN, birth date and sex is reason
// mrn_birth_sex; equal MRN otherwise is mrn_only (A11: two patients and a
// proposal, never one patient). A pair is proposed once per reason; a
// rejected pair is not proposed again. It returns a signal per new row.
func Propose(ctx context.Context, tx pgx.Tx, conn connector.Config, patient uuid.UUID) ([]signals.Signal, error) {
	ns := conn.Settings["mrn_namespace"]
	if ns == "" {
		return nil, nil
	}
	var mrn *string
	var birth *time.Time
	var sx, status string
	if err := tx.QueryRow(ctx, `SELECT p.mrn, p.birth_date, p.sex, p.status FROM emr.patients p
		WHERE p.hospital_id = $1 AND p.id = $2`, conn.HospitalID, patient).Scan(&mrn, &birth, &sx, &status); err != nil {
		return nil, err
	}
	if mrn == nil || status != "active" {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT p.id,
			(p.birth_date IS NOT NULL AND p.birth_date = $5 AND p.sex = $6 AND p.sex <> 'unknown')
		FROM emr.patients p
		JOIN emr.patient_links l ON l.patient_id = p.id AND l.hospital_id = p.hospital_id
		JOIN public.emr_connections c ON c.id = l.connection_id AND c.hospital_id = l.hospital_id
		WHERE p.hospital_id = $1 AND p.status = 'active' AND p.mrn = $2 AND p.id <> $3
		  AND l.connection_id <> $4 AND c.settings ->> 'mrn_namespace' = $7`,
		conn.HospitalID, *mrn, patient, conn.ConnectionID, birth, sx, ns)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id   uuid.UUID
		full bool
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		return c, r.Scan(&c.id, &c.full)
	})
	if err != nil {
		return nil, err
	}
	var out []signals.Signal
	for _, c := range cands {
		reason := "mrn_only"
		if c.full {
			reason = "mrn_birth_sex"
		}
		s, err := propose(ctx, tx, conn, c.id, patient, reason, "possible_match")
		if err != nil {
			return nil, err
		}
		out = append(out, s...)
	}
	return out, nil
}

// SourceMerge records a source's merge (FHIR Patient.link replaced-by) of
// patient into the same connection's targetSourceID as a proposal only
// (A12, A31): nothing moves until a hospital admin confirms.
func SourceMerge(ctx context.Context, tx pgx.Tx, conn connector.Config, patient uuid.UUID, targetSourceID string) ([]signals.Signal, error) {
	target, err := Resolve(ctx, tx, conn, targetSourceID, nil, nil)
	if err != nil {
		return nil, err
	}
	if target.ID == patient || target.Status != "active" {
		return nil, nil
	}
	return propose(ctx, tx, conn, patient, target.ID, "source_merge", "source_merge")
}

func propose(ctx context.Context, tx pgx.Tx, conn connector.Config, a, b uuid.UUID, reason, kind string) ([]signals.Signal, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO emr.possible_matches (hospital_id, patient_a, patient_b, reason, connection_id)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING RETURNING id`, conn.HospitalID, a, b, reason, conn.ConnectionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []signals.Signal{{Kind: kind, ConnectionID: conn.ConnectionID, RefID: id}}, nil
}
