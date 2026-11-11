package normalise

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/codes"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/index"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/signals"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
)

// The closed reject codes (migration 00023).
const (
	rejUnreadable = "unreadable_data"
	rejScope      = "out_of_scope"
	rejNoPatient  = "no_patient"
	rejExcluded   = "excluded"
)

type outcome struct {
	reject  string
	created []uuid.UUID
	signals []signals.Signal
}

// errReject carries a reject code out of a record's handler.
type errReject string

func (e errReject) Error() string { return "normalise: rejected " + string(e) }

// record normalises one raw record inside a savepoint: a constraint the
// database refuses rejects that record only (unreadable_data), never the
// batch. Any other database error is returned (the batch is retried).
func (n *Normaliser) record(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) (outcome, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return outcome{}, err
	}
	// The patient first (design §10.6): a record naming an excluded patient,
	// or one merged into an excluded patient, is dropped as excluded with its
	// payload cleared, whatever else is wrong with it.
	var out outcome
	excluded, err := namesExcluded(ctx, sp, cfg, r)
	if err == nil && excluded {
		err = errReject(rejExcluded)
	} else if err == nil {
		out, err = n.dispatch(ctx, sp, cfg, r)
	}
	var rej errReject
	var pe *pgconn.PgError
	switch {
	case err == nil:
		if err := sp.Commit(ctx); err != nil {
			return outcome{}, err
		}
	case errors.As(err, &rej):
		_ = sp.Rollback(ctx)
		out = outcome{reject: string(rej)}
	case errors.As(err, &pe) && strings.HasPrefix(pe.Code, "23"):
		_ = sp.Rollback(ctx)
		out = outcome{reject: rejUnreadable}
	default:
		_ = sp.Rollback(ctx)
		return outcome{}, err
	}
	if out.reject != "" {
		// An excluded patient's record is not kept at all (design §10.6).
		q := `UPDATE emr.raw_records SET status = 'rejected', reject_code = $3, normalised_at = now() WHERE hospital_id = $1 AND id = $2`
		if out.reject == rejExcluded {
			q = `UPDATE emr.raw_records SET status = 'rejected', reject_code = $3, payload = NULL, normalised_at = now()
				WHERE hospital_id = $1 AND id = $2`
		}
		_, err := tx.Exec(ctx, q, cfg.HospitalID, r.id, out.reject)
		return out, err
	}
	_, err = tx.Exec(ctx, `UPDATE emr.raw_records SET status = 'normalised', normalised_at = now() WHERE hospital_id = $1 AND id = $2`,
		cfg.HospitalID, r.id)
	return out, err
}

// namesExcluded reports whether r is about a patient already excluded: the
// Patient resource itself, or a record whose patient field names one. It
// creates nothing. A link left on a merged patient is followed to the
// patient it was merged into.
func namesExcluded(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) (bool, error) {
	src := r.sourceID
	if r.kind != "patient" {
		src = str(r.payload, "patient")
	}
	if src == "" {
		return false, nil
	}
	var excluded bool
	err := tx.QueryRow(ctx, `WITH RECURSIVE chain(id, status, merged_into, depth) AS (
			SELECT p.id, p.status, p.merged_into, 0 FROM emr.patient_links l
			JOIN emr.patients p ON p.id = l.patient_id AND p.hospital_id = l.hospital_id
			WHERE l.hospital_id = $1 AND l.connection_id = $2 AND l.source_patient_id = $3
			UNION ALL
			SELECT p.id, p.status, p.merged_into, c.depth + 1 FROM chain c
			JOIN emr.patients p ON p.id = c.merged_into AND p.hospital_id = $1
			WHERE c.depth < 16)
		SELECT EXISTS (SELECT 1 FROM chain WHERE status = 'excluded')`,
		cfg.HospitalID, cfg.ConnectionID, src).Scan(&excluded)
	return excluded, err
}

func (n *Normaliser) dispatch(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) (outcome, error) {
	if r.payload == nil {
		return outcome{}, errReject(rejUnreadable)
	}
	switch r.kind {
	case "patient":
		return n.patient(ctx, tx, cfg, r)
	case "location":
		return outcome{}, n.location(ctx, tx, cfg, r)
	case "encounter":
		return n.encounter(ctx, tx, cfg, r)
	case "observation":
		return n.observation(ctx, tx, cfg, r)
	case "medication":
		return n.medication(ctx, tx, cfg, r)
	}
	return outcome{}, errReject(rejScope)
}

// --- payload access -----------------------------------------------------

func str(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

func codingsOf(p map[string]any, k string) []connector.Coding {
	list, _ := p[k].([]any)
	var out []connector.Coding
	for _, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, connector.Coding{System: str(m, "system"), Code: str(m, "code"), Display: str(m, "display")})
	}
	return out
}

func sep(cfg connector.Config) rune {
	if cfg.Settings["decimal_separator"] == "," {
		return ','
	}
	return '.'
}

// patientOf resolves the record's patient (by source id, within the
// connection); an excluded patient's record is rejected.
func patientOf(ctx context.Context, tx pgx.Tx, cfg connector.Config, p map[string]any) (uuid.UUID, error) {
	src := str(p, "patient")
	if src == "" {
		return uuid.Nil, errReject(rejNoPatient)
	}
	pt, err := index.Resolve(ctx, tx, cfg, src, nil, nil)
	if errors.Is(err, index.ErrSourceID) {
		return uuid.Nil, errReject(rejNoPatient)
	}
	if err != nil {
		return uuid.Nil, err
	}
	if pt.Status != "active" {
		return uuid.Nil, errReject(rejExcluded)
	}
	return pt.ID, nil
}

// timeOf reads an optional source time: a dateTime in UTC, or nil for a
// date only or none; a time in the future or unreadable rejects.
func (n *Normaliser) timeOf(p map[string]any, k string) (*time.Time, *time.Time, error) {
	s := str(p, k)
	if s == "" {
		return nil, nil, nil
	}
	t, dateOnly, err := ParseFHIRTime(s, time.UTC)
	if err != nil || CheckFuture(t, n.now()) != nil {
		return nil, nil, errReject(rejUnreadable)
	}
	if dateOnly {
		return nil, &t, nil
	}
	return &t, nil, nil
}

// --- provenance and versions --------------------------------------------

type versioned struct {
	table  string // one of the three constants below, never input
	cfg    connector.Config
	r      raw
	status string
}

const (
	tObservations = "emr.observations"
	tMedications  = "emr.medications"
	tEncounters   = "emr.encounters"
)

// prov is the provenance columns' values, in the order the inserts list
// them after the table's own columns.
func (n *Normaliser) prov(cfg connector.Config, r raw) []any {
	return []any{r.id, "connector", cfg.ConnectionID, r.connType, r.connVersion, r.sourceID, r.version, r.updated,
		r.received, n.Maps.Version(), Version}
}

const provCols = `raw_id, source_kind, connection_id, connector_type, connector_version, source_id, source_version,
	source_updated_at, received_at, mapping_version, normaliser_version, superseded_by`

// current finds the current version of the record's source id, if any.
func current(ctx context.Context, tx pgx.Tx, v versioned) (uuid.UUID, *time.Time, bool, error) {
	var id uuid.UUID
	var updated *time.Time
	err := tx.QueryRow(ctx, `SELECT id, source_updated_at FROM `+v.table+`
		WHERE hospital_id = $1 AND connection_id = $2 AND source_id = $3 AND superseded_by IS NULL
		ORDER BY created_at DESC LIMIT 1`, v.cfg.HospitalID, v.cfg.ConnectionID, v.r.sourceID).Scan(&id, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil, false, nil
	}
	return id, updated, err == nil, err
}

// write inserts a new version (design §8.2): an entered-in-error or
// cancelled version marks the current row instead; an older version that
// arrives late is stored already superseded; otherwise the current row is
// superseded by the new one. It returns the row the rules should look at.
func write(ctx context.Context, tx pgx.Tx, v versioned, insert func(supersededBy *uuid.UUID) (uuid.UUID, error)) (uuid.UUID, error) {
	cur, curUpdated, found, err := current(ctx, tx, v)
	if err != nil {
		return uuid.Nil, err
	}
	if found && (v.status == "entered-in-error" || v.status == "cancelled") {
		_, err := tx.Exec(ctx, `UPDATE `+v.table+` SET status = $3 WHERE hospital_id = $1 AND id = $2`, v.cfg.HospitalID, cur, v.status)
		return cur, err
	}
	var by *uuid.UUID
	if found && curUpdated != nil && v.r.updated != nil && curUpdated.After(*v.r.updated) {
		by = &cur
	}
	id, err := insert(by)
	if err != nil {
		return uuid.Nil, err
	}
	if found && by == nil {
		if _, err := tx.Exec(ctx, `UPDATE `+v.table+` SET superseded_by = $3 WHERE hospital_id = $1 AND id = $2`,
			v.cfg.HospitalID, cur, id); err != nil {
			return uuid.Nil, err
		}
	}
	if by != nil {
		return cur, nil
	}
	return id, nil
}

func (n *Normaliser) unmapped(ctx context.Context, tx pgx.Tx, cfg connector.Config, kind, system, code, display string) error {
	if code == "" {
		return nil
	}
	n.metrics().Unmapped.Add(1)
	var d *string
	if display != "" {
		d = &display
	}
	_, err := tx.Exec(ctx, `INSERT INTO emr.unmapped_codes (hospital_id, connection_id, kind, local_system, local_code, local_display)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (connection_id, kind, local_system, local_code)
		DO UPDATE SET count = emr.unmapped_codes.count + 1, last_seen = now(),
		              local_display = coalesce(excluded.local_display, emr.unmapped_codes.local_display)`,
		cfg.HospitalID, cfg.ConnectionID, kind, system, code, d)
	return err
}

// --- the kinds ------------------------------------------------------------

func (n *Normaliser) patient(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) (outcome, error) {
	f := index.PatientFields{
		DisplayName: sink.CleanText(strings.TrimSpace(str(r.payload, "given")+" "+str(r.payload, "family")), 120),
		MRN:         str(r.payload, "mrn"),
		Sex:         str(r.payload, "sex"),
	}
	if b := str(r.payload, "birth_date"); b != "" {
		if t, dateOnly, err := ParseFHIRTime(b, time.UTC); err == nil && dateOnly && CheckFuture(t, n.now()) == nil {
			f.BirthDate = &t
		}
	}
	id := r.id
	pt, err := index.Resolve(ctx, tx, cfg, r.sourceID, &f, &id)
	if err != nil {
		return outcome{}, err
	}
	if pt.Status != "active" {
		return outcome{}, errReject(rejExcluded)
	}
	var out outcome
	sigs, err := index.Propose(ctx, tx, cfg, pt.ID)
	if err != nil {
		return outcome{}, err
	}
	out.signals = append(out.signals, sigs...)
	if ext, _ := r.payload["link_replaced_by_external"].(bool); ext {
		// A merge into another source's patient: counted, never followed.
		n.metrics().ExternalLinks.Add(1)
	}
	if target := str(r.payload, "link_replaced_by"); target != "" && target != r.sourceID {
		sigs, err := index.SourceMerge(ctx, tx, cfg, pt.ID, target)
		if err != nil {
			return outcome{}, err
		}
		out.signals = append(out.signals, sigs...)
	}
	return out, nil
}

var unitKinds = map[string]string{"ward": "ward", "clinic": "clinic", "emergency": "emergency"}

func (n *Normaliser) location(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) error {
	label := str(r.payload, "name")
	if label == "" {
		return errReject(rejUnreadable)
	}
	kind, ok := unitKinds[str(r.payload, "type")]
	if !ok {
		kind = "other"
	}
	_, err := tx.Exec(ctx, `INSERT INTO emr.units (hospital_id, connection_id, source_location_id, label, kind, raw_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (connection_id, source_location_id) DO UPDATE SET label = excluded.label, kind = excluded.kind,
		    raw_id = excluded.raw_id, updated_at = now()`, cfg.HospitalID, cfg.ConnectionID, r.sourceID, label, kind, r.id)
	return err
}

var encounterClass = map[string]string{"inpatient": "inpatient", "IMP": "inpatient", "outpatient": "outpatient",
	"AMB": "outpatient", "emergency": "emergency", "EMER": "emergency"}

var encounterStatus = []string{"planned", "arrived", "in-progress", "finished", "cancelled", "entered-in-error"}

func (n *Normaliser) encounter(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) (outcome, error) {
	class, ok := encounterClass[str(r.payload, "class")]
	if !ok {
		return outcome{}, errReject(rejScope)
	}
	status := str(r.payload, "status")
	if !contains(encounterStatus, status) {
		return outcome{}, errReject(rejUnreadable)
	}
	pid, err := patientOf(ctx, tx, cfg, r.payload)
	if err != nil {
		return outcome{}, err
	}
	start, _, err := n.timeOf(r.payload, "period_start")
	if err != nil {
		return outcome{}, err
	}
	end, _, err := n.timeOf(r.payload, "period_end")
	if err != nil {
		return outcome{}, err
	}
	var unit *uuid.UUID
	if loc := str(r.payload, "location"); loc != "" {
		var u uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM emr.units WHERE hospital_id = $1 AND connection_id = $2 AND source_location_id = $3`,
			cfg.HospitalID, cfg.ConnectionID, loc).Scan(&u)
		if err == nil {
			unit = &u
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return outcome{}, err
		}
	}
	bed := nullable(str(r.payload, "bed_label"))
	v := versioned{table: tEncounters, cfg: cfg, r: r, status: status}
	_, err = write(ctx, tx, v, func(by *uuid.UUID) (uuid.UUID, error) {
		var id uuid.UUID
		args := append([]any{cfg.HospitalID, pid, class, status, start, end, unit, bed}, n.prov(cfg, r)...)
		err := tx.QueryRow(ctx, `INSERT INTO emr.encounters (hospital_id, patient_id, class, status, period_start, period_end,
			unit_id, bed_label, `+provCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
			$17, $18, $19, $20) RETURNING id`, append(args, by)...).Scan(&id)
		return id, err
	})
	return outcome{}, err
}

var obsStatus = []string{"preliminary", "final", "amended", "corrected", "cancelled", "entered-in-error"}
var sourceFlags = []string{"L", "H", "LL", "HH", "A", "AA", "N"}

func (n *Normaliser) observation(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) (outcome, error) {
	p := r.payload
	category := str(p, "category")
	if category != "laboratory" && category != "vital-signs" {
		return outcome{}, errReject(rejScope)
	}
	status := str(p, "status")
	if !contains(obsStatus, status) {
		return outcome{}, errReject(rejUnreadable)
	}
	cs := codingsOf(p, "codings")
	if len(cs) == 0 {
		return outcome{}, errReject(rejUnreadable)
	}
	// LOINC as given if the table knows it; otherwise the connection's
	// reviewed map, by code. Never by display text.
	loinc := ""
	local := cs[0]
	for _, c := range cs {
		if c.System == codes.LOINCSystem {
			if _, ok := n.Tables.LOINC(c.Code); ok && loinc == "" {
				loinc = c.Code
			}
			continue
		}
		local = c
		if l, ok := n.Maps.LOINC(cfg.ConnectionID, c.System, c.Code); ok && loinc == "" {
			loinc = l
		}
	}
	effAt, effDate, err := n.timeOf(p, "effective")
	if err != nil {
		return outcome{}, err
	}
	if effAt == nil && effDate == nil {
		return outcome{}, errReject(rejUnreadable)
	}
	issued, _, err := n.timeOf(p, "issued")
	if err != nil {
		return outcome{}, err
	}
	// The value: a number read strictly, or text; a comparator keeps it
	// text (never a number equal to its bound).
	var valueNum, valueSI *float64
	var comparator, valueText *string
	if q := str(p, "value_quantity"); q != "" {
		v, cmp, err := ParseDecimal(q, sep(cfg))
		if err != nil {
			return outcome{}, errReject(rejUnreadable)
		}
		if cmp != "" {
			t := strings.TrimSpace(q)
			comparator, valueText = &cmp, &t
		} else {
			valueNum = &v
		}
	} else if t := str(p, "value_text"); t != "" {
		valueText = &t
	}
	unitSource := str(p, "unit")
	var unitUCUM *string
	if unitSource != "" {
		if u, ok := n.Maps.ParseUnit(unitSource); ok {
			unitUCUM = &u
		} else if err := n.unmapped(ctx, tx, cfg, "unit", "unit", truncate(unitSource, 64), ""); err != nil {
			return outcome{}, err
		}
	}
	if valueNum != nil && unitUCUM != nil && loinc != "" {
		if si, ok := n.Maps.Convert(loinc, *valueNum, *unitUCUM); ok {
			valueSI = &si
		}
	}
	standardised := loinc != "" && (valueNum == nil || valueSI != nil)
	if loinc == "" {
		if err := n.unmapped(ctx, tx, cfg, "observation", local.System, local.Code, local.Display); err != nil {
			return outcome{}, err
		}
	}
	refLow := n.refValue(p, "ref_low", cfg)
	refHigh := n.refValue(p, "ref_high", cfg)
	var flag *string
	if f := str(p, "interpretation"); contains(sourceFlags, f) {
		flag = &f
	}
	pid, err := patientOf(ctx, tx, cfg, p)
	if err != nil {
		return outcome{}, err
	}
	var enc *uuid.UUID
	if src := str(p, "encounter"); src != "" {
		var e uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id FROM emr.encounters WHERE hospital_id = $1 AND connection_id = $2 AND source_id = $3
			AND superseded_by IS NULL ORDER BY created_at DESC LIMIT 1`, cfg.HospitalID, cfg.ConnectionID, src).Scan(&e)
		if err == nil {
			enc = &e
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return outcome{}, err
		}
	}
	var effD *string
	if effDate != nil {
		d := effDate.Format("2006-01-02")
		effD = &d
	}
	v := versioned{table: tObservations, cfg: cfg, r: r, status: status}
	id, err := write(ctx, tx, v, func(by *uuid.UUID) (uuid.UUID, error) {
		var id uuid.UUID
		args := append([]any{cfg.HospitalID, pid, enc, category, nullable(loinc), nullable(local.System), nullable(local.Code),
			nullable(local.Display), valueNum, comparator, valueText, unitUCUM, nullable(unitSource), valueSI, refLow, refHigh,
			nullable(str(p, "ref_text")), flag, status, effAt, effD, issued, standardised}, n.prov(cfg, r)...)
		err := tx.QueryRow(ctx, `INSERT INTO emr.observations (hospital_id, patient_id, encounter_id, category, loinc,
			local_system, local_code, local_display, value_num, comparator, value_text, unit_ucum, unit_source, value_si,
			ref_low, ref_high, ref_text, source_flag, status, effective_at, effective_date, issued_at, standardised, `+provCols+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21::date, $22,
			$23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35) RETURNING id`, append(args, by)...).Scan(&id)
		return id, err
	})
	if err != nil {
		return outcome{}, err
	}
	return outcome{created: []uuid.UUID{id}}, nil
}

// refValue reads a reference range bound; one it can't read is left out
// (the source's own text stays in ref_text).
func (n *Normaliser) refValue(p map[string]any, k string, cfg connector.Config) *float64 {
	s := str(p, k)
	if s == "" {
		return nil
	}
	v, cmp, err := ParseDecimal(s, sep(cfg))
	if err != nil || cmp != "" {
		return nil
	}
	return &v
}

var medStatus = []string{"active", "on-hold", "stopped", "completed", "cancelled", "entered-in-error"}

func (n *Normaliser) medication(ctx context.Context, tx pgx.Tx, cfg connector.Config, r raw) (outcome, error) {
	p := r.payload
	kind := str(p, "kind")
	if kind != "request" && kind != "statement" {
		return outcome{}, errReject(rejScope)
	}
	status := str(p, "status")
	if !contains(medStatus, status) {
		return outcome{}, errReject(rejUnreadable)
	}
	cs := codingsOf(p, "codings")
	atc := ""
	var local connector.Coding
	for _, c := range cs {
		if c.System == codes.ATCSystem {
			if _, ok := n.Tables.ATC(c.Code); ok && atc == "" {
				atc = c.Code
			}
			continue
		}
		if local.Code == "" {
			local = c
		}
		if a, ok := n.Maps.ATC(cfg.ConnectionID, c.System, c.Code); ok && atc == "" {
			atc = a
		}
	}
	if local.Code == "" && len(cs) > 0 {
		local = cs[0]
	}
	display := local.Display
	if display == "" {
		display = str(p, "med_text")
	}
	if len(cs) == 0 && display == "" {
		return outcome{}, errReject(rejUnreadable)
	}
	if atc == "" {
		if err := n.unmapped(ctx, tx, cfg, "medication", local.System, local.Code, display); err != nil {
			return outcome{}, err
		}
	}
	var dose *float64
	if d := str(p, "dose_value"); d != "" {
		v, cmp, err := ParseDecimal(d, sep(cfg))
		if err != nil || cmp != "" {
			return outcome{}, errReject(rejUnreadable)
		}
		dose = &v
	}
	var doseUnit *string
	if u := str(p, "dose_unit"); u != "" {
		if ucum, ok := n.Maps.ParseUnit(u); ok {
			doseUnit = &ucum
		} else if err := n.unmapped(ctx, tx, cfg, "unit", "unit", truncate(u, 64), ""); err != nil {
			return outcome{}, err
		}
	}
	start, _, err := n.timeOf(p, "period_start")
	if err != nil {
		return outcome{}, err
	}
	if start == nil {
		if start, _, err = n.timeOf(p, "authored"); err != nil {
			return outcome{}, err
		}
	}
	end, _, err := n.timeOf(p, "period_end")
	if err != nil {
		return outcome{}, err
	}
	pid, err := patientOf(ctx, tx, cfg, p)
	if err != nil {
		return outcome{}, err
	}
	v := versioned{table: tMedications, cfg: cfg, r: r, status: status}
	id, err := write(ctx, tx, v, func(by *uuid.UUID) (uuid.UUID, error) {
		var id uuid.UUID
		args := append([]any{cfg.HospitalID, pid, kind, nullable(atc), nullable(local.System), nullable(local.Code),
			nullable(display), dose, doseUnit, nullable(str(p, "route")), nullable(str(p, "timing_text")), status, start, end,
			atc != ""}, n.prov(cfg, r)...)
		err := tx.QueryRow(ctx, `INSERT INTO emr.medications (hospital_id, patient_id, kind, atc, local_system, local_code,
			local_display, dose_value, dose_unit_ucum, route, frequency_text, status, start_at, end_at, standardised, `+provCols+`)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23,
			$24, $25, $26, $27) RETURNING id`, append(args, by)...).Scan(&id)
		return id, err
	})
	if err != nil {
		return outcome{}, err
	}
	return outcome{created: []uuid.UUID{id}}, nil
}

func contains(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
