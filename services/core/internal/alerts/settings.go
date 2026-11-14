package alerts

// GET and PATCH /hospital/alert-settings (design §12.2, §16.2; plan Task
// 32): hospital admins. The connection is status only, by the hospital's
// own labels (no address, type or error code); a PATCH saves exactly one
// section at the version it was read at (409 conflict otherwise), and is
// audited with counts only. No step-up (Plan 13 D8).

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Bounds on a PATCH.
const (
	maxUnits      = 1000
	maxRecipients = 200
)

// SourceOut is one source of the hospital, by its label.
type SourceOut struct {
	Label        string  `json:"label"`
	Status       string  `json:"status"`
	LastUpdateAt *string `json:"last_update_at"`
}

// ConnectionOut is the hospital's records status: the worst of its sources,
// not_connected with none.
type ConnectionOut struct {
	Status  string      `json:"status"`
	Sources []SourceOut `json:"sources"`
}

// UnitOut is one unit the sources know, and whether it is covered.
type UnitOut struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Kind        string  `json:"kind"`
	SourceLabel *string `json:"source_label"`
	Covered     bool    `json:"covered"`
}

// CategoryOut is one category and its level.
type CategoryOut struct {
	ID      string  `json:"id"`
	Label   string  `json:"label"`
	Example *string `json:"example"`
	Level   string  `json:"level"`
}

// ClinicianOut is a named recipient.
type ClinicianOut struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Specialty   *string `json:"specialty"`
}

// RecipientsOut is the queue switch and the named clinicians (active
// clinicians of the hospital only).
type RecipientsOut struct {
	Queue      bool           `json:"queue"`
	Clinicians []ClinicianOut `json:"clinicians"`
}

// PersonOut names a person by display name.
type PersonOut struct {
	DisplayName string `json:"display_name"`
}

// SettingsOut is AlertSettingsOut.
type SettingsOut struct {
	Connection ConnectionOut `json:"connection"`
	Units      []UnitOut     `json:"units"`
	Categories []CategoryOut `json:"categories"`
	Recipients RecipientsOut `json:"recipients"`
	UpdatedBy  *PersonOut    `json:"updated_by"`
	UpdatedAt  *string       `json:"updated_at"`
	Version    int           `json:"version"`
	sourceByID map[uuid.UUID]string
}

// statusRank orders source statuses from best to worst.
var statusRank = map[string]int{"connected": 0, "off": 1, "problems": 2}

// sources reads the hospital's sources (emr_hospital_sources()).
func sources(ctx context.Context, tx pgx.Tx) (ConnectionOut, map[uuid.UUID]string, error) {
	out := ConnectionOut{Status: "not_connected", Sources: []SourceOut{}}
	labels := map[uuid.UUID]string{}
	rows, err := tx.Query(ctx, `SELECT connection_id, source_label, status, last_update_at FROM emr_hospital_sources()`)
	if err != nil {
		return out, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var s SourceOut
		var at *time.Time
		if err := rows.Scan(&id, &s.Label, &s.Status, &at); err != nil {
			return out, nil, err
		}
		s.LastUpdateAt = stampPtr(at)
		labels[id] = s.Label
		if out.Status == "not_connected" || statusRank[s.Status] > statusRank[out.Status] {
			out.Status = s.Status
		}
		out.Sources = append(out.Sources, s)
	}
	return out, labels, rows.Err()
}

// loadSettings reads the hospital's settings, with every default filled in.
func loadSettings(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (SettingsOut, error) {
	out := SettingsOut{Units: []UnitOut{}, Categories: []CategoryOut{},
		Recipients: RecipientsOut{Queue: true, Clinicians: []ClinicianOut{}}}
	var err error
	if out.Connection, out.sourceByID, err = sources(ctx, tx); err != nil {
		return out, err
	}
	var by *string
	var at *time.Time
	err = tx.QueryRow(ctx, `SELECT s.queue, s.version, u.display_name, s.updated_at FROM alert_settings s
		LEFT JOIN users u ON u.id = s.updated_by AND u.hospital_id = s.hospital_id WHERE s.hospital_id = $1`, hid).
		Scan(&out.Recipients.Queue, &out.Version, &by, &at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if by != nil {
		out.UpdatedBy = &PersonOut{DisplayName: *by}
	}
	out.UpdatedAt = stampPtr(at)

	rows, err := tx.Query(ctx, `SELECT u.id, u.label, u.kind, u.connection_id, coalesce(a.covered, true)
		FROM emr.units u LEFT JOIN alert_units a ON a.hospital_id = u.hospital_id AND a.unit_id = u.id
		WHERE u.hospital_id = $1 ORDER BY u.label, u.id`, hid)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var u UnitOut
		var id, conn uuid.UUID
		if err := rows.Scan(&id, &u.Label, &u.Kind, &conn, &u.Covered); err != nil {
			rows.Close()
			return out, err
		}
		u.ID = id.String()
		if l, ok := out.sourceByID[conn]; ok {
			u.SourceLabel = &l
		}
		out.Units = append(out.Units, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	levels, err := savedLevels(ctx, tx, hid)
	if err != nil {
		return out, err
	}
	for _, c := range Categories {
		ex := c.Example
		lvl, ok := levels[c.ID]
		if !ok {
			lvl = DefaultLevel(c.ID)
		}
		out.Categories = append(out.Categories, CategoryOut{ID: c.ID, Label: c.Label, Example: &ex, Level: lvl})
	}

	rows, err = tx.Query(ctx, `SELECT u.id, u.display_name, u.specialty FROM alert_recipients a
		JOIN users u ON u.id = a.user_id AND u.hospital_id = a.hospital_id AND u.is_active
		JOIN user_roles r ON r.user_id = u.id AND r.role = 'clinician'
		WHERE a.hospital_id = $1 ORDER BY u.display_name, u.id`, hid)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ClinicianOut
		var id uuid.UUID
		if err := rows.Scan(&id, &c.DisplayName, &c.Specialty); err != nil {
			return out, err
		}
		c.ID = id.String()
		out.Recipients.Clinicians = append(out.Recipients.Clinicians, c)
	}
	return out, rows.Err()
}

func savedLevels(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (map[string]string, error) {
	rows, err := tx.Query(ctx, `SELECT category, level FROM alert_levels WHERE hospital_id = $1`, hid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var c, l string
		if err := rows.Scan(&c, &l); err != nil {
			return nil, err
		}
		out[c] = l
	}
	return out, rows.Err()
}

// settingsGet is GET /hospital/alert-settings. Not audited: settings, no
// patient data.
func (d Deps) settingsGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out SettingsOut
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = loadSettings(r.Context(), tx, p.HospitalID)
		return err
	}); err != nil {
		d.fail(w, r, p, "settings read", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// settingsPatch is the PATCH body: version and exactly one section.
type settingsPatch struct {
	Version *int `json:"version"`
	Units   *struct {
		Covered *[]string `json:"covered"`
	} `json:"units"`
	Rules *struct {
		Levels map[string]string `json:"levels"`
	} `json:"rules"`
	Recipients *struct {
		Queue        *bool     `json:"queue"`
		ClinicianIDs *[]string `json:"clinician_ids"`
	} `json:"recipients"`
}

// ids parses canonical uuids, deduplicated, at most max.
func ids(in []string, max int) ([]uuid.UUID, bool) {
	if len(in) > max {
		return nil, false
	}
	out := make([]uuid.UUID, 0, len(in))
	for _, s := range in {
		u, ok := canonicalID(s)
		if !ok {
			return nil, false
		}
		if !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out, true
}

// check validates the body's shape (422 before any read).
func (b settingsPatch) check() bool {
	n := 0
	for _, set := range []bool{b.Units != nil, b.Rules != nil, b.Recipients != nil} {
		if set {
			n++
		}
	}
	if n != 1 || b.Version == nil || *b.Version < 0 {
		return false
	}
	switch {
	case b.Units != nil:
		if b.Units.Covered == nil {
			return false
		}
		_, ok := ids(*b.Units.Covered, maxUnits)
		return ok
	case b.Rules != nil:
		if len(b.Rules.Levels) == 0 {
			return false
		}
		for c, l := range b.Rules.Levels {
			if _, ok := CategoryByID(c); !ok || !slices.Contains(Levels, l) {
				return false
			}
		}
		return true
	default:
		if b.Recipients.Queue == nil || b.Recipients.ClinicianIDs == nil {
			return false
		}
		_, ok := ids(*b.Recipients.ClinicianIDs, maxRecipients)
		return ok
	}
}

// settingsPatch is PATCH /hospital/alert-settings.
func (d Deps) settingsPatch(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var body settingsPatch
	if !httpx.Decode(w, r, &body) {
		return
	}
	if !body.check() {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if body.Recipients != nil && !*body.Recipients.Queue && len(*body.Recipients.ClinicianIDs) == 0 {
		httpx.Error(w, r, "no_recipients")
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var out SettingsOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		if _, err := tx.Exec(ctx, `INSERT INTO alert_settings (hospital_id) VALUES ($1) ON CONFLICT (hospital_id) DO NOTHING`,
			p.HospitalID); err != nil {
			return err
		}
		var version int
		var queue bool
		if err := tx.QueryRow(ctx, `SELECT version, queue FROM alert_settings WHERE hospital_id = $1 FOR UPDATE`,
			p.HospitalID).Scan(&version, &queue); err != nil {
			return err
		}
		if version != *body.Version {
			return errConflict
		}
		var e audit.Event
		var err error
		switch {
		case body.Units != nil:
			e, err = saveUnits(ctx, tx, p.HospitalID, *body.Units.Covered)
		case body.Rules != nil:
			e, err = saveLevels(ctx, tx, p.HospitalID, body.Rules.Levels)
		default:
			queue = *body.Recipients.Queue
			e, err = saveRecipients(ctx, tx, p.HospitalID, queue, *body.Recipients.ClinicianIDs)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_settings SET queue = $2, version = version + 1, updated_by = $3,
			updated_at = now() WHERE hospital_id = $1`, p.HospitalID, queue, p.UserID); err != nil {
			return err
		}
		e.ActorID, e.HospitalID, e.RequestID = p.UserID.String(), p.HospitalID.String(), rid
		e.Entity, e.EntityID = "hospital", p.HospitalID.String()
		if err := audit.Write(ctx, tx, e); err != nil {
			return err
		}
		out, err = loadSettings(ctx, tx, p.HospitalID)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "settings save", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// saveUnits covers exactly the listed units: every one must be a unit of
// the hospital (422 otherwise).
func saveUnits(ctx context.Context, tx pgx.Tx, hid uuid.UUID, raw []string) (audit.Event, error) {
	covered, _ := ids(raw, maxUnits)
	rows, err := tx.Query(ctx, `SELECT id FROM emr.units WHERE hospital_id = $1`, hid)
	if err != nil {
		return audit.Event{}, err
	}
	known, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return audit.Event{}, err
	}
	for _, u := range covered {
		if !slices.Contains(known, u) {
			return audit.Event{}, errInvalid
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO alert_units (hospital_id, unit_id, covered)
		SELECT $1, u.id, u.id = ANY ($2::uuid[]) FROM emr.units u WHERE u.hospital_id = $1
		ON CONFLICT (hospital_id, unit_id) DO UPDATE SET covered = EXCLUDED.covered`, hid, covered); err != nil {
		return audit.Event{}, err
	}
	return audit.Event{Action: "alert_settings.units_changed",
		Payload: map[string]any{"covered": len(covered), "units": len(known)}}, nil
}

// saveLevels sets the given categories' levels.
func saveLevels(ctx context.Context, tx pgx.Tx, hid uuid.UUID, levels map[string]string) (audit.Event, error) {
	saved, err := savedLevels(ctx, tx, hid)
	if err != nil {
		return audit.Event{}, err
	}
	changed := 0
	for _, c := range Categories {
		l, ok := levels[c.ID]
		if !ok {
			continue
		}
		was, set := saved[c.ID]
		if !set {
			was = DefaultLevel(c.ID)
		}
		if was != l {
			changed++
		}
		if _, err := tx.Exec(ctx, `INSERT INTO alert_levels (hospital_id, category, level) VALUES ($1, $2, $3)
			ON CONFLICT (hospital_id, category) DO UPDATE SET level = EXCLUDED.level`, hid, c.ID, l); err != nil {
			return audit.Event{}, err
		}
	}
	return audit.Event{Action: "alert_settings.rules_changed", Payload: map[string]any{"levels_changed": changed}}, nil
}

// saveRecipients replaces the named clinicians: each must be an active
// clinician of the hospital (A15; the table's trigger checks it again).
// Clinicians taken off the list have their open alerts that no other active
// clinician was sent moved to the queue (design §12.2).
func saveRecipients(ctx context.Context, tx pgx.Tx, hid uuid.UUID, queue bool, raw []string) (audit.Event, error) {
	want, _ := ids(raw, maxRecipients)
	if !queue && len(want) == 0 {
		return audit.Event{}, errNoRecips
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT u.id) FROM users u
		JOIN user_roles r ON r.user_id = u.id AND r.role = 'clinician'
		WHERE u.id = ANY ($1::uuid[]) AND u.hospital_id = $2 AND u.is_active`, want, hid).Scan(&n); err != nil {
		return audit.Event{}, err
	}
	if n != len(want) {
		return audit.Event{}, errInvalid
	}
	rows, err := tx.Query(ctx, `DELETE FROM alert_recipients WHERE hospital_id = $1 AND NOT (user_id = ANY ($2::uuid[]))
		RETURNING user_id`, hid, want)
	if err != nil {
		return audit.Event{}, err
	}
	removed, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return audit.Event{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO alert_recipients (hospital_id, user_id) SELECT $1, unnest($2::uuid[])
		ON CONFLICT (hospital_id, user_id) DO NOTHING`, hid, want); err != nil {
		return audit.Event{}, err
	}
	requeued := 0
	if len(removed) > 0 {
		if err := tx.QueryRow(ctx, `SELECT alerts_requeue_removed($1::uuid[])`, removed).Scan(&requeued); err != nil {
			return audit.Event{}, err
		}
	}
	return audit.Event{Action: "alert_settings.recipients_changed",
		Payload: map[string]any{"recipients": len(want), "queue": queue, "requeued": requeued}}, nil
}
