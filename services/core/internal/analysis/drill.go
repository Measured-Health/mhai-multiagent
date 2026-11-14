package analysis

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

type smallGuarded = smallgroup.Guarded

// The de-identified drill-down (design §5.6; plan Task 13, departures 13
// and 19). A drill key is derived when a body is served, never stored:
// HMAC(drill key; hospital ‖ period id ‖ the context's hash ‖ view ‖ row
// key), 43 characters of base64url. It is set only on a shown count of 5
// or more of a group the rows describe (age band, sex or setting; a pair;
// a finding), in a context whose filters the rows describe (no location,
// specialty or source): pids are stable within a period, so a list split
// by a dimension the rows don't show would tell which patients are in a
// group the aggregate rule may hide. Prevalence counts cases, not patients
// (a month's cases are its blocks' sum), so no list could equal its cell:
// it has no drill either.

// DrillGroup is one group a list may be opened on.
type DrillGroup struct {
	View disclose.View
	// RowKey names the row as disclose.Drillable does.
	RowKey string
	// Group is the group as mhai_analysis_open takes it.
	Group map[string]string
	// Count is the shown cell: the list's count must equal it.
	Count int64
}

// rowDims are the filters a row shows, and so the only ones a drill may
// be opened under.
var rowDims = map[int]bool{disclose.DimCondition: true, disclose.DimAgeBand: true, disclose.DimSex: true, disclose.DimSetting: true}

// DrillableContext reports whether lists may be opened under f.
func DrillableContext(f disclose.Filters) bool {
	for d, v := range f {
		if v != "" && !rowDims[d] {
			return false
		}
	}
	return true
}

var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// DrillGroups are body b's drillable groups, under f: none for a body
// hidden whole or a context the rows don't describe.
func DrillGroups(b disclose.Body, f disclose.Filters) []DrillGroup {
	if b.Whole || !DrillableContext(f) {
		return nil
	}
	var out []DrillGroup
	add := func(v disclose.View, rowKey string, g map[string]string, val *int64) {
		if val == nil || !disclose.Drillable(b, v, rowKey) {
			return
		}
		out = append(out, DrillGroup{View: v, RowKey: rowKey, Group: g, Count: *val})
	}
	for _, p := range []struct {
		name, dim string
		rows      []disclose.Part[smallGuarded]
	}{{"by_age", "age_band", b.ByAge}, {"by_sex", "sex", b.BySex}, {"by_setting", "setting", b.BySetting}} {
		for _, r := range p.rows {
			add(disclose.Affected, p.name+":"+r.Key, map[string]string{"kind": "part", "dim": p.dim, "key": r.Key}, r.Patients.Value)
		}
	}
	for _, r := range b.Pairs {
		if uuidShape.MatchString(r.A) && uuidShape.MatchString(r.B) && r.A < r.B {
			add(disclose.CoOccurring, r.A+"|"+r.B, map[string]string{"kind": "pair", "a": r.A, "b": r.B}, r.Patients.Value)
		}
	}
	for _, r := range b.Findings {
		if r.Name == "" || uuidShape.MatchString(r.Name) {
			add(disclose.Findings, r.Name+"|"+r.Flag, map[string]string{"kind": "finding", "condition": r.Name, "flag": r.Flag}, r.Patients.Value)
		}
	}
	return out
}

// pidAnywhere is the pseudonym's shape anywhere in a string (mhai-web's
// PSEUDONYM_IN): no drill key may hold one (P7).
var pidAnywhere = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)

// DrillKey is the key of group g of the context (hospital, period, f): ""
// when there are no keys, or when the key would hold the pseudonym's
// shape (the cell then has no drill).
func DrillKey(k *Keys, hospital, period uuid.UUID, f disclose.Filters, g DrillGroup) string {
	if k == nil {
		return ""
	}
	key := k.Drill([]byte("analysis/drill/v1"), hospital[:], period[:], Hash(f), []byte(g.View), []byte(g.RowKey))
	if pidAnywhere.MatchString(key) {
		return ""
	}
	return key
}

// FindDrill is the group of body b whose key, for this context, is drill.
func FindDrill(k *Keys, hospital, period uuid.UUID, f disclose.Filters, b disclose.Body, drill string) (DrillGroup, bool) {
	var found DrillGroup
	ok := false
	for _, g := range DrillGroups(b, f) {
		key := DrillKey(k, hospital, period, f, g)
		if key != "" && hmac.Equal([]byte(key), []byte(drill)) {
			found, ok = g, true
		}
	}
	return found, ok
}

// SetDrills is a stored view with the drill key of each drillable count
// set (every other drill stays null). Keys unchanged, the same bytes every
// time (P3).
func SetDrills(view json.RawMessage, v disclose.View, b disclose.Body, f disclose.Filters, key func(DrillGroup) string) (json.RawMessage, error) {
	groups := map[string]DrillGroup{}
	for _, g := range DrillGroups(b, f) {
		if g.View == v {
			groups[g.RowKey] = g
		}
	}
	if len(groups) == 0 {
		return view, nil
	}
	var o map[string]any
	dec := json.NewDecoder(bytes.NewReader(view))
	dec.UseNumber()
	if err := dec.Decode(&o); err != nil {
		return nil, ErrFailed
	}
	// The JSON lists are the record's, in its order (renderView).
	set := func(list string, keys []string) error {
		rows, _ := o[list].([]any)
		if len(rows) != len(keys) {
			return ErrFailed
		}
		for i, r := range rows {
			g, ok := groups[keys[i]]
			if !ok {
				continue
			}
			m, _ := r.(map[string]any)
			cell, _ := m["patients"].(map[string]any)
			if cell == nil {
				return ErrFailed
			}
			if k := key(g); k != "" {
				cell["drill"] = k
			}
		}
		return nil
	}
	var err error
	switch v {
	case disclose.Affected:
		for _, p := range []struct {
			name string
			rows []disclose.Part[smallGuarded]
		}{{"by_age", b.ByAge}, {"by_sex", b.BySex}, {"by_setting", b.BySetting}} {
			var keys []string
			for _, r := range p.rows {
				keys = append(keys, p.name+":"+r.Key)
			}
			if err = set(p.name, keys); err != nil {
				return nil, err
			}
		}
	case disclose.CoOccurring:
		var keys []string
		for _, r := range b.Pairs {
			keys = append(keys, r.A+"|"+r.B)
		}
		err = set("pairs", keys)
	case disclose.Findings:
		var keys []string
		for _, r := range b.Findings {
			keys = append(keys, r.Name+"|"+r.Flag)
		}
		err = set("findings", keys)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(o)
}

// The list's errors (each maps to one problem code in internal/analysis/routes).
var (
	// ErrTooSmall: the group isn't the shown cell's, or under 5 (422 too_small).
	ErrTooSmall = errors.New("analysis: not a listable group")
	// ErrPatientCap: the person's distinct patients this quarter (429).
	ErrPatientCap = errors.New("analysis: patient cap")
)

// List is one de-identified list to open.
type List struct {
	View   uuid.UUID
	Period Period
	F      disclose.Filters
	G      DrillGroup
	Drill  string
	// Now pins the clock (nil: the database's).
	Now *time.Time
}

// PatientRow is one row as sent: exactly the contract's keys.
type PatientRow struct {
	PID        string        `json:"pid"`
	AgeBand    *string       `json:"age_band"`
	Sex        string        `json:"sex"`
	Setting    *string       `json:"setting"`
	Conditions []ConditionOf `json:"conditions"`
	Findings   []FindingOf   `json:"findings"`
}

// ConditionOf is the contract's ConditionRef.
type ConditionOf struct {
	ID    string  `json:"id"`
	Name  *string `json:"name"`
	ICD10 *string `json:"icd10"`
}

// FindingOf is a row's finding. source_label is always null: a row shows
// no source (departure 19).
type FindingOf struct {
	Name        *string `json:"name"`
	Flag        *string `json:"flag"`
	SourceLabel *string `json:"source_label"`
}

// Open opens l in tx (the request's, as db.WithHospitalUser: the person
// is the database's mhai_current_user()): the group recounted from the
// frozen period and checked against the cell, the patients reserved
// against the person's quarter, the list and its rows recorded, the pids
// drawn. The caller appends analysis.patients_viewed in the same
// transaction: a list without it is refused at commit.
func Open(ctx context.Context, tx pgx.Tx, l List) ([]PatientRow, error) {
	g, err := json.Marshal(l.G.Group)
	if err != nil {
		return nil, ErrFailed
	}
	drill := sha256.Sum256([]byte(l.Drill))
	rows, err := tx.Query(ctx, `SELECT ord, pid, age_band, sex, setting, conditions::text, findings::text
		FROM mhai_analysis_open($1, $2, $3::jsonb, $4::jsonb, $5, $6, $7, $8, $9)`,
		l.View, l.Period.ID, filtersJSON(l.F), string(g), string(l.G.View), drill[:], l.G.Count, disclose.PatientCap, l.Now)
	if err != nil {
		return nil, listErr(err)
	}
	var got []rawRow
	for rows.Next() {
		var r rawRow
		var ord int
		if err := rows.Scan(&ord, &r.row.PID, &r.row.AgeBand, &r.row.Sex, &r.row.Setting, &r.conds, &r.finds); err != nil {
			rows.Close()
			return nil, listErr(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, listErr(err)
	}
	return buildRows(ctx, tx, got)
}

// rawRow is a list row as the database sends it.
type rawRow struct {
	row          PatientRow
	conds, finds []byte
}

// buildRows is the rows as sent: the conditions and findings named from
// the hospital's dictionary, at most 10 of each.
func buildRows(ctx context.Context, tx pgx.Tx, got []rawRow) ([]PatientRow, error) {
	n, err := loadNames(ctx, tx)
	if err != nil {
		return nil, ErrFailed
	}
	out := make([]PatientRow, 0, len(got))
	for _, r := range got {
		var conds []string
		var finds []struct{ Condition, Flag string }
		if json.Unmarshal(r.conds, &conds) != nil || json.Unmarshal(r.finds, &finds) != nil ||
			len(conds) > disclose.RowItemsMax || len(finds) > disclose.RowItemsMax {
			return nil, ErrFailed
		}
		row := r.row
		row.Conditions, row.Findings = []ConditionOf{}, []FindingOf{}
		for _, c := range conds {
			cn := n.conds[c]
			row.Conditions = append(row.Conditions, ConditionOf{ID: c, Name: cn.name, ICD10: cn.icd10})
		}
		for _, f := range finds {
			var fo FindingOf
			if f.Condition != "" {
				fo.Name = n.conds[f.Condition].name
			}
			if f.Flag != "" {
				fl := f.Flag
				fo.Flag = &fl
			}
			row.Findings = append(row.Findings, fo)
		}
		out = append(out, row)
	}
	return out, nil
}

// filtersJSON is the filters by name (cells.FiltersJSON's form; the raw
// reader isn't imported here, only its input shape repeated).
func filtersJSON(f disclose.Filters) string {
	m := map[string]string{}
	for d, v := range f {
		if v != "" {
			m[disclose.DimNames[d]] = v
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// FiltersCodes is the filters as the audit payload carries them.
func FiltersCodes(f disclose.Filters) map[string]any {
	m := map[string]any{}
	for d, v := range f {
		if v != "" {
			m[disclose.DimNames[d]] = v
		}
	}
	return m
}

// listErr maps the open's refusals to their errors, and anything else to
// ErrFailed (never the driver's text).
func listErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "P0001" {
		switch pe.Message {
		case "too_small":
			return ErrTooSmall
		case "patient_cap":
			return ErrPatientCap
		}
	}
	if errors.As(err, &pe) && pe.Code == "22023" {
		return ErrTooSmall
	}
	return failed(err)
}

// PersonTx runs fn as the verified principal's hospital and person
// (db.WithHospitalUser: mhai_current_user() is the token's user, never a
// value from the request), for the list's functions, which act as that
// person. A platform principal, or none, is refused.
func PersonTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	p, ok := authz.FromContext(ctx)
	if !ok || p.Platform || p.HospitalID == uuid.Nil || p.UserID == uuid.Nil {
		return authz.ErrNoPrincipal
	}
	return db.WithHospitalUser(ctx, pool, p.HospitalID, p.UserID, fn)
}

// Stored is the stored body of (p, f), never computed: ok false when
// there is none.
func (e *Engine) Stored(ctx context.Context, p Period, f disclose.Filters) (Stored, bool, error) {
	return e.get(ctx, p, f)
}
