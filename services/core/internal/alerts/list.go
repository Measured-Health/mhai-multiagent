package alerts

// GET /alerts and GET /alerts/unread-count (design §12.3, §15.3, §16.3,
// §16.4; plan Task 33): a clinician sees an alert on the hospital's queue,
// or delivered to them by name, never one routed elsewhere or of another
// hospital (A14, A17). Who sees it was fixed when it was raised: a later
// settings change never hides it (Review Focus 4). Every page carries the
// sources' status (behind when stale or paused, A23, A42), so an empty list
// is never read as "nothing wrong" while a source is behind. A page of
// alerts served is audited (alerts.read, the ids and the filter codes).

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/recordlink"
)

// Page sizes.
const (
	DefaultLimit = 50
	MaxLimit     = 100
)

// FindingOut is the evidence of the alert's primary result.
type FindingOut struct {
	Name           string  `json:"name"`
	Value          *string `json:"value"`
	Unit           *string `json:"unit"`
	Flag           *string `json:"flag"`
	ReferenceRange *string `json:"reference_range"`
	RecordedAt     *string `json:"recorded_at"`
	CodeSystem     string  `json:"code_system"`
	Standardised   bool    `json:"standardised"`
}

// EvidenceOut is one row behind a likely cause: a label, a value and a
// time, never an identifier.
type EvidenceOut struct {
	Label      string  `json:"label"`
	Value      *string `json:"value"`
	RecordedAt *string `json:"recorded_at"`
}

// CauseOut is a likely cause (a suggestion, never a diagnosis).
type CauseOut struct {
	Label      string        `json:"label"`
	Confidence string        `json:"confidence"`
	Evidence   []EvidenceOut `json:"evidence"`
}

// LocationOut is where the patient is; nil reads "Location not recorded".
type LocationOut struct {
	Kind   string  `json:"kind"`
	Ward   *string `json:"ward,omitempty"`
	Bed    *string `json:"bed,omitempty"`
	Clinic *string `json:"clinic,omitempty"`
}

// PatientOut is the alert's patient. mrn is null for a patient with none.
type PatientOut struct {
	FullName string       `json:"full_name"`
	MRN      *string      `json:"mrn"`
	Location *LocationOut `json:"location"`
}

// RoutingOut is who the alert was sent to, for the caller.
type RoutingOut struct {
	ToYou bool `json:"to_you"`
	Queue bool `json:"queue"`
}

// CategoryRef names the alert's category.
type CategoryRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// AmendedOut is what an amended result replaced.
type AmendedOut struct {
	PreviousValue *string `json:"previous_value"`
	AmendedAt     *string `json:"amended_at"`
}

// AlertOut is one alert.
type AlertOut struct {
	ID            string       `json:"id"`
	Severity      string       `json:"severity"`
	Status        string       `json:"status"`
	Finding       FindingOut   `json:"finding"`
	LikelyCauses  []CauseOut   `json:"likely_causes"`
	Patient       PatientOut   `json:"patient"`
	Age           *int         `json:"age"`
	Sex           *string      `json:"sex"`
	SourceLabel   *string      `json:"source_label"`
	EMRLink       *string      `json:"emr_link"`
	HandledBy     *PersonOut   `json:"handled_by"`
	HandledAt     *string      `json:"handled_at"`
	DismissReason *string      `json:"dismiss_reason"`
	CreatedAt     string       `json:"created_at"`
	Category      *CategoryRef `json:"category"`
	Routing       RoutingOut   `json:"routing"`
	ArrivedLate   bool         `json:"arrived_late"`
	Preliminary   bool         `json:"preliminary"`
	Amended       *AmendedOut  `json:"amended"`
	Withdrawn     *string      `json:"withdrawn"`
}

// SourceStatusOut is one switched-on source, current or behind.
type SourceStatusOut struct {
	Label        string  `json:"label"`
	Status       string  `json:"status"`
	LastUpdateAt *string `json:"last_update_at"`
}

// PageOut is AlertPageOut.
type PageOut struct {
	Alerts        []AlertOut        `json:"alerts"`
	NextCursor    *string           `json:"next_cursor"`
	Sources       []string          `json:"sources"`
	Queue         bool              `json:"queue"`
	SourcesStatus []SourceStatusOut `json:"sources_status"`
	Grouped       *struct{}         `json:"grouped"`
}

// filters are a list request's filters.
type filters struct {
	Status, Severity, Source, SentTo string
}

var listKeys = []string{"status", "severity", "source", "sent_to", "cursor", "limit"}

// parseList reads the query: each key at most once, closed values.
func parseList(r *http.Request) (f filters, cursor string, limit int, ok bool) {
	q := r.URL.Query()
	limit = DefaultLimit
	for k, v := range q {
		if !slices.Contains(listKeys, k) || len(v) != 1 {
			return f, "", 0, false
		}
	}
	f.Status, f.Severity, f.Source, f.SentTo = q.Get("status"), q.Get("severity"), q.Get("source"), q.Get("sent_to")
	if (q.Has("status") && !slices.Contains([]string{"new", "handled", "dismissed"}, f.Status)) ||
		(q.Has("severity") && !slices.Contains([]string{"critical", "urgent", "info"}, f.Severity)) ||
		(q.Has("sent_to") && f.SentTo != "you" && f.SentTo != "queue") ||
		(q.Has("source") && (f.Source == "" || len(f.Source) > 40)) {
		return f, "", 0, false
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > MaxLimit || strconv.Itoa(n) != q.Get("limit") {
			return f, "", 0, false
		}
		limit = n
	}
	return f, q.Get("cursor"), limit, true
}

// cursorKeyLabel derives the list cursor's HMAC key from the master key,
// apart from every other key derived from it.
const cursorKeyLabel = "mhai alerts cursor key v1"

// cursors makes and checks one session's cursors: hex(created_at as unix
// nanoseconds, 8 bytes || alert id, 16 bytes || tag, 16 bytes), the tag
// an HMAC over the session, the filters and the position. Opaque, bound to
// the session and the filters (a forged, another session's or another
// filter's cursor is refused).
type cursors struct {
	key     []byte
	session string
}

func newCursors(master []byte, session string) (cursors, bool) {
	if len(master) < 32 || session == "" {
		return cursors{}, false
	}
	k, err := hkdf.Key(sha256.New, master, nil, cursorKeyLabel, 32)
	if err != nil {
		return cursors{}, false
	}
	return cursors{key: k, session: session}, true
}

func (c cursors) tag(pos []byte, f filters) []byte {
	raw, _ := json.Marshal(f)
	m := hmac.New(sha256.New, c.key)
	for _, part := range [][]byte{[]byte("alerts.cursor.v1"), []byte(c.session), raw, pos} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		m.Write(n[:])
		m.Write(part)
	}
	return m.Sum(nil)[:16]
}

func (c cursors) encode(at time.Time, id uuid.UUID, f filters) string {
	pos := make([]byte, 8, 24)
	binary.BigEndian.PutUint64(pos, uint64(at.UnixNano()))
	pos = append(pos, id[:]...)
	return hex.EncodeToString(append(pos, c.tag(pos, f)...))
}

func (c cursors) decode(s string, f filters) (time.Time, uuid.UUID, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 40 || hex.EncodeToString(b) != s || !hmac.Equal(b[24:], c.tag(b[:24], f)) {
		return time.Time{}, uuid.Nil, false
	}
	id, err := uuid.FromBytes(b[8:24])
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(b[:8]))).UTC(), id, true
}

// visible is the SQL condition for "the caller sees alert a": $1 the
// hospital, $2 the caller. An alert whose patient an exclusion removed is
// no longer listed.
const visible = `a.hospital_id = $1 AND a.patient_id IS NOT NULL
	AND (a.routed_queue OR EXISTS (SELECT 1 FROM alert_deliveries d
	                               WHERE d.hospital_id = a.hospital_id AND d.alert_id = a.id AND d.user_id = $2))`

// alertSelect reads everything an AlertOut shows, for alert a (with
// visible's $1 and $2).
const alertSelect = `SELECT a.id, a.severity, a.status, a.category, a.routed_queue,
	EXISTS (SELECT 1 FROM alert_deliveries d WHERE d.hospital_id = a.hospital_id AND d.alert_id = a.id AND d.user_id = $2),
	a.preliminary, a.arrived_late, a.amended, a.withdrawn, h.display_name, a.handled_at, a.dismiss_reason, a.created_at,
	a.connection_id, p.display_name, p.mrn, p.birth_date, p.sex, f.causes,
	o.local_display, o.loinc, o.value_num::text, o.comparator, o.value_text, o.unit_ucum, o.unit_source,
	o.ref_low::text, o.ref_high::text, o.ref_text, o.source_flag, o.effective_at, o.standardised,
	loc.class, loc.bed_label, loc.unit_label
	FROM alerts a
	JOIN emr.patients p ON p.hospital_id = a.hospital_id AND p.id = a.patient_id
	LEFT JOIN users h ON h.id = a.handled_by AND h.hospital_id = a.hospital_id
	LEFT JOIN emr.findings f ON f.hospital_id = a.hospital_id AND f.id = a.finding_id
	LEFT JOIN emr.observations o ON o.hospital_id = f.hospital_id AND o.id = f.primary_observation_id
	LEFT JOIN LATERAL (SELECT e.class, e.bed_label, u.label AS unit_label FROM emr.encounters e
	                   LEFT JOIN emr.units u ON u.hospital_id = e.hospital_id AND u.id = e.unit_id
	                   WHERE e.hospital_id = a.hospital_id AND e.patient_id = a.patient_id AND e.superseded_by IS NULL
	                     AND e.status IN ('arrived', 'in-progress')
	                   ORDER BY e.period_start DESC NULLS LAST, e.id LIMIT 1) loc ON true`

// alertRow is one scanned row and the ids its evidence needs.
type alertRow struct {
	out       AlertOut
	createdAt time.Time
	conn      *uuid.UUID
	causes    []causeJSON
}

// causeJSON is a finding's stored cause (the emr service's shape).
type causeJSON struct {
	Label        string      `json:"label"`
	Confidence   string      `json:"confidence"`
	Observations []uuid.UUID `json:"observation_ids"`
	Medications  []uuid.UUID `json:"medication_ids"`
}

func scanAlert(row pgx.Row, now time.Time) (alertRow, error) {
	var x alertRow
	var id uuid.UUID
	var category string
	var routedQueue, toYou bool
	var amended []byte
	var handledBy *string
	var handledAt *time.Time
	var name *string
	var birth *time.Time
	var sex string
	var causes []byte
	var display, loinc, valueNum, comparator, valueText, unitUCUM, unitSource *string
	var refLow, refHigh, refText, flag *string
	var effective *time.Time
	var standardised *bool
	var class, bed, unitLabel *string
	err := row.Scan(&id, &x.out.Severity, &x.out.Status, &category, &routedQueue, &toYou, &x.out.Preliminary,
		&x.out.ArrivedLate, &amended, &x.out.Withdrawn, &handledBy, &handledAt, &x.out.DismissReason, &x.createdAt,
		&x.conn, &name, &x.out.Patient.MRN, &birth, &sex, &causes, &display, &loinc, &valueNum, &comparator, &valueText,
		&unitUCUM, &unitSource, &refLow, &refHigh, &refText, &flag, &effective, &standardised, &class, &bed, &unitLabel)
	if err != nil {
		return x, err
	}
	x.out.ID = id.String()
	x.out.CreatedAt = stamp(x.createdAt)
	x.out.HandledAt = stampPtr(handledAt)
	if handledBy != nil {
		x.out.HandledBy = &PersonOut{DisplayName: *handledBy}
	}
	if c, ok := CategoryByID(category); ok {
		x.out.Category = &CategoryRef{ID: c.ID, Label: c.Label}
	}
	x.out.Routing = RoutingOut{ToYou: toYou, Queue: routedQueue}
	if name != nil {
		x.out.Patient.FullName = *name
	}
	x.out.Sex = &sex
	if birth != nil {
		age := ageAt(*birth, now)
		x.out.Age = &age
	}
	x.out.Patient.Location = location(class, bed, unitLabel)
	x.out.Finding = finding(display, loinc, valueNum, comparator, valueText, unitUCUM, unitSource, refLow, refHigh,
		refText, flag, effective, standardised)
	x.out.Amended = amendedOut(amended)
	x.out.LikelyCauses = []CauseOut{}
	if len(causes) > 0 {
		_ = json.Unmarshal(causes, &x.causes)
	}
	return x, nil
}

// ageAt is the whole years from birth to now.
func ageAt(birth, now time.Time) int {
	y := now.Year() - birth.Year()
	if now.Month() < birth.Month() || (now.Month() == birth.Month() && now.Day() < birth.Day()) {
		y--
	}
	if y < 0 {
		return 0
	}
	return y
}

func location(class, bed, unit *string) *LocationOut {
	if class == nil {
		return nil
	}
	switch *class {
	case "inpatient":
		if unit == nil {
			return nil
		}
		return &LocationOut{Kind: "admitted", Ward: unit, Bed: bed}
	case "outpatient":
		return &LocationOut{Kind: "outpatient", Clinic: unit}
	case "emergency":
		return &LocationOut{Kind: "emergency"}
	}
	return nil
}

// flags maps the source's (or rule's) flag to the contract's (design
// §16.3); severity carries critical versus urgent.
var flags = map[string]string{"L": "low", "LL": "low", "H": "high", "HH": "high", "A": "abnormal", "AA": "abnormal"}

func finding(display, loinc, valueNum, comparator, valueText, unitUCUM, unitSource, refLow, refHigh, refText, flag *string,
	effective *time.Time, standardised *bool) FindingOut {
	f := FindingOut{Name: "Result", CodeSystem: "local"}
	switch {
	case display != nil && *display != "":
		f.Name = *display
	case loinc != nil:
		f.Name = *loinc
	}
	if loinc != nil {
		f.CodeSystem = "loinc"
	}
	switch {
	case comparator != nil && valueText != nil:
		v := *comparator + *valueText
		f.Value = &v
	case valueNum != nil:
		f.Value = valueNum
	default:
		f.Value = valueText
	}
	f.Unit = unitSource
	if f.Unit == nil {
		f.Unit = unitUCUM
	}
	switch {
	case refText != nil:
		f.ReferenceRange = refText
	case refLow != nil && refHigh != nil:
		r := *refLow + "–" + *refHigh
		f.ReferenceRange = &r
	}
	if flag != nil {
		if v, ok := flags[*flag]; ok {
			f.Flag = &v
		}
	}
	f.RecordedAt = stampPtr(effective)
	f.Standardised = standardised != nil && *standardised
	return f
}

func amendedOut(raw []byte) *AmendedOut {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	out := &AmendedOut{}
	if s, ok := m["previous_value"].(string); ok && len(s) <= 120 {
		out.PreviousValue = &s
	}
	if s, ok := m["amended_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			out.AmendedAt = stampPtr(&t)
		}
	}
	return out
}

// fillAlerts adds what each alert needs beyond its row: the source label,
// the record link, and the likely causes with their evidence.
func fillAlerts(ctx context.Context, tx pgx.Tx, rows []alertRow, labels map[uuid.UUID]string) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	var obs, meds []uuid.UUID
	for i := range rows {
		id, _ := uuid.Parse(rows[i].out.ID)
		ids = append(ids, id)
		if rows[i].conn != nil {
			if l, ok := labels[*rows[i].conn]; ok {
				rows[i].out.SourceLabel = &l
			}
		}
		for _, c := range rows[i].causes {
			obs = append(obs, c.Observations...)
			meds = append(meds, c.Medications...)
		}
	}
	links := map[string]string{}
	lr, err := tx.Query(ctx, `SELECT alert_id, template, base_url, source_patient_id FROM emr_alert_links($1::uuid[])`, ids)
	if err != nil {
		return err
	}
	for lr.Next() {
		var id uuid.UUID
		var tmpl, base, spid string
		if err := lr.Scan(&id, &tmpl, &base, &spid); err != nil {
			lr.Close()
			return err
		}
		if link, ok := recordlink.Build(tmpl, base, spid); ok {
			links[id.String()] = link
		}
	}
	lr.Close()
	if err := lr.Err(); err != nil {
		return err
	}
	evidence := map[uuid.UUID]EvidenceOut{}
	if len(obs) > 0 {
		er, err := tx.Query(ctx, `SELECT o.id, coalesce(o.local_display, o.loinc, 'Result'),
			coalesce(o.comparator || o.value_text, o.value_num::text, o.value_text), coalesce(o.unit_source, o.unit_ucum),
			o.effective_at FROM emr.observations o WHERE o.hospital_id = $1 AND o.id = ANY ($2::uuid[])`,
			authzHospital(ctx), obs)
		if err != nil {
			return err
		}
		for er.Next() {
			var id uuid.UUID
			var e EvidenceOut
			var v, unit *string
			var at *time.Time
			if err := er.Scan(&id, &e.Label, &v, &unit, &at); err != nil {
				er.Close()
				return err
			}
			if v != nil && unit != nil {
				s := *v + " " + *unit
				v = &s
			}
			e.Value, e.RecordedAt = v, stampPtr(at)
			evidence[id] = e
		}
		er.Close()
		if err := er.Err(); err != nil {
			return err
		}
	}
	if len(meds) > 0 {
		mr, err := tx.Query(ctx, `SELECT m.id, coalesce(m.local_display, m.atc, 'Medication'), m.start_at
			FROM emr.medications m WHERE m.hospital_id = $1 AND m.id = ANY ($2::uuid[])`, authzHospital(ctx), meds)
		if err != nil {
			return err
		}
		for mr.Next() {
			var id uuid.UUID
			var e EvidenceOut
			var at *time.Time
			if err := mr.Scan(&id, &e.Label, &at); err != nil {
				mr.Close()
				return err
			}
			e.RecordedAt = stampPtr(at)
			evidence[id] = e
		}
		mr.Close()
		if err := mr.Err(); err != nil {
			return err
		}
	}
	for i := range rows {
		if l, ok := links[rows[i].out.ID]; ok {
			rows[i].out.EMRLink = &l
		}
		for _, c := range rows[i].causes {
			// No high confidence in v1 (design §11.3): anything but caution
			// or review is not shown.
			if (c.Confidence != "caution" && c.Confidence != "review") || c.Label == "" || len(rows[i].out.LikelyCauses) == 3 {
				continue
			}
			co := CauseOut{Label: c.Label, Confidence: c.Confidence, Evidence: []EvidenceOut{}}
			for _, id := range slices.Concat(c.Observations, c.Medications) {
				if e, ok := evidence[id]; ok && len(co.Evidence) < 3 {
					co.Evidence = append(co.Evidence, e)
				}
			}
			rows[i].out.LikelyCauses = append(rows[i].out.LikelyCauses, co)
		}
	}
	return nil
}

// authzHospital is the request's hospital (the principal's).
func authzHospital(ctx context.Context) uuid.UUID {
	p, _ := authz.FromContext(ctx)
	return p.HospitalID
}

// loadAlert reads one alert the caller sees (errNotFound otherwise).
func loadAlert(ctx context.Context, tx pgx.Tx, hid, me, id uuid.UUID, now time.Time) (AlertOut, error) {
	x, err := scanAlert(tx.QueryRow(ctx, alertSelect+` WHERE `+visible+` AND a.id = $3`, hid, me, id), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertOut{}, errNotFound
	}
	if err != nil {
		return AlertOut{}, err
	}
	_, labels, err := sources(ctx, tx)
	if err != nil {
		return AlertOut{}, err
	}
	rows := []alertRow{x}
	if err := fillAlerts(ctx, tx, rows, labels); err != nil {
		return AlertOut{}, err
	}
	return rows[0].out, nil
}

// sourcesStatus is the switched-on sources, current or behind
// (emr_sources_status(), 00027).
func sourcesStatus(ctx context.Context, tx pgx.Tx) ([]SourceStatusOut, error) {
	rows, err := tx.Query(ctx, `SELECT source_label, status, last_update_at FROM emr_sources_status() ORDER BY source_label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceStatusOut{}
	for rows.Next() {
		var s SourceStatusOut
		var at *time.Time
		if err := rows.Scan(&s.Label, &s.Status, &at); err != nil {
			return nil, err
		}
		s.LastUpdateAt = stampPtr(at)
		out = append(out, s)
	}
	return out, rows.Err()
}

// list is GET /alerts.
func (d Deps) list(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	f, cursor, limit, ok := parseList(r)
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	cur, haveKey := newCursors(d.MasterKey, p.SessionID)
	var after *time.Time
	var afterID uuid.UUID
	if cursor != "" {
		t, id, ok := cur.decode(cursor, f)
		if !haveKey || !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		after, afterID = &t, id
	}
	now := d.now()
	out := PageOut{Alerts: []AlertOut{}, Sources: []string{}, Queue: true}
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		conn, labels, err := sources(ctx, tx)
		if err != nil {
			return err
		}
		for _, s := range conn.Sources {
			if !slices.Contains(out.Sources, s.Label) {
				out.Sources = append(out.Sources, s.Label)
			}
		}
		if out.SourcesStatus, err = sourcesStatus(ctx, tx); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT queue FROM alert_settings WHERE hospital_id = $1`, p.HospitalID).Scan(&out.Queue)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		q := alertSelect + ` WHERE ` + visible
		args := []any{p.HospitalID, p.UserID}
		add := func(cond string, v any) {
			args = append(args, v)
			q += " AND " + cond + "$" + strconv.Itoa(len(args))
		}
		if f.Status != "" {
			add("a.status = ", f.Status)
		}
		if f.Severity != "" {
			add("a.severity = ", f.Severity)
		}
		if f.Source != "" {
			conns := []uuid.UUID{}
			for id, l := range labels {
				if l == f.Source {
					conns = append(conns, id)
				}
			}
			args = append(args, conns)
			q += " AND a.connection_id = ANY ($" + strconv.Itoa(len(args)) + "::uuid[])"
		}
		switch f.SentTo {
		case "you":
			q += ` AND EXISTS (SELECT 1 FROM alert_deliveries d WHERE d.hospital_id = a.hospital_id AND d.alert_id = a.id AND d.user_id = $2)`
		case "queue":
			q += ` AND a.routed_queue AND NOT EXISTS (SELECT 1 FROM alert_deliveries d
				WHERE d.hospital_id = a.hospital_id AND d.alert_id = a.id AND d.user_id = $2)`
		}
		if after != nil {
			args = append(args, *after, afterID)
			q += " AND (a.created_at, a.id) < ($" + strconv.Itoa(len(args)-1) + ", $" + strconv.Itoa(len(args)) + ")"
		}
		args = append(args, limit+1)
		q += " ORDER BY a.created_at DESC, a.id DESC LIMIT $" + strconv.Itoa(len(args))
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		var page []alertRow
		for rows.Next() {
			x, err := scanAlert(rows, now)
			if err != nil {
				rows.Close()
				return err
			}
			page = append(page, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page) > limit {
			page = page[:limit]
			if haveKey {
				last := page[len(page)-1]
				id, _ := uuid.Parse(last.out.ID)
				c := cur.encode(last.createdAt, id, f)
				out.NextCursor = &c
			}
		}
		if err := fillAlerts(ctx, tx, page, labels); err != nil {
			return err
		}
		served := make([]string, 0, len(page))
		for _, x := range page {
			out.Alerts = append(out.Alerts, x.out)
			served = append(served, x.out.ID)
		}
		if len(served) == 0 {
			return nil
		}
		// Who saw which alert (design §12.6, A21): the ids served and the
		// filter codes; the source label is free text, so only whether one
		// was given.
		payload := map[string]any{"alert_ids": served, "source": f.Source != "", "page": cursor != ""}
		for k, v := range map[string]string{"status": f.Status, "severity": f.Severity, "sent_to": f.SentTo} {
			if v != "" {
				payload[k] = v
			}
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "alerts.read", Entity: "user", EntityID: p.UserID.String(),
			Payload: payload})
	})
	if err != nil {
		d.fail(w, r, p, "list", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// UnreadOut is the unread count.
type UnreadOut struct {
	Count int `json:"count"`
}

// unreadCount is GET /alerts/unread-count: new alerts the caller sees (by
// name or on the queue), not yet handled by anyone.
func (d Deps) unreadCount(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out UnreadOut
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT count(*) FROM alerts a WHERE `+visible+` AND a.status = 'new'`,
			p.HospitalID, p.UserID).Scan(&out.Count)
	}); err != nil {
		d.fail(w, r, p, "unread count", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
