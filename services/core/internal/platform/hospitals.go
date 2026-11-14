package platform

// The platform app's hospitals list and a hospital's page (mhai-web Plan
// 7b; platform admin design §7.1, §7.3, §13):
//
//   - GET /platform/hospitals?status&q&cursor: platform admins and
//     auditors; pages of 50 hospitals by name, then id. q is a part of the
//     name or short code, any case (1 to 100 characters, no control
//     characters); status one of the six. The cursor is opaque
//     (base64url of the last row's name and id); one that doesn't decode
//     to a name and a canonical uuid is 422.
//   - GET /platform/hospitals/{id}: the same readers; the summary and the
//     page's parts: the active hospital admins to contact and the open
//     legal holds in short form.
//   - GET /platform/hospitals/{id}/sso: the same readers; core keeps no
//     single sign-on per hospital, so it is always off and empty.
//
// What core doesn't keep is sent empty, never invented: country "" and
// email_domains [] (no columns; hospitals are made by the provisioning
// CLI), version 1 (no hospital write is served), sso_enabled false, and
// deactivation, first_admin and removal null (no deactivation record,
// invite or removal request exists in core). A legal hold always keeps
// every record (scope from and to null); its review date is design §4's
// default, six months after it was placed (in the hospital's time zone),
// and it is review_due from that day. Core lifts a hold at once (no grace
// period), so only open holds keep records and are listed.
//
// Not recorded: platform configuration and staff contacts, not clinical
// data (as the model policy read). The admins' names and emails are never
// logged.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// HospitalsPage is the list's page size.
	HospitalsPage = 50
	// hospitalQueryMax is q's longest, in characters.
	hospitalQueryMax = 100
	// hospitalCursorMax bounds the cursor before it is decoded.
	hospitalCursorMax = 1024
	// HoldReviewMonths is a hold's review date: this many months after it
	// was placed (mhai-web REVIEW_MONTHS).
	HoldReviewMonths = 6
)

// HospitalStatuses are hospitals.status's values (00001's CHECK).
var HospitalStatuses = []string{"active", "setup_incomplete", "deactivated", "removal_pending", "removal_scheduled", "removed"}

func registerHospitals(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin, authz.RoleAuditor}}
	authz.Route(m, http.MethodGet, "/platform/hospitals", read, http.HandlerFunc(d.hospitalList))
	authz.Route(m, http.MethodGet, "/platform/hospitals/{id}", read, http.HandlerFunc(d.hospitalGet))
	authz.Route(m, http.MethodGet, "/platform/hospitals/{id}/sso", read, http.HandlerFunc(d.hospitalSSO))
}

// HospitalSummary is a hospital as the list shows it.
type HospitalSummary struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	ShortCode        string  `json:"short_code"`
	Status           string  `json:"status"`
	StaffCount       int64   `json:"staff_count"`
	SSOEnabled       bool    `json:"sso_enabled"`
	CreatedAt        string  `json:"created_at"`
	LastActivityAt   *string `json:"last_activity_at"`
	LegalHoldsActive int64   `json:"legal_holds_active"`
}

// HoldScope is the records a hold keeps: both null, all of them.
type HoldScope struct {
	From *string `json:"from"`
	To   *string `json:"to"`
}

// HoldShort is a hold as a hospital lists it.
type HoldShort struct {
	ID        string    `json:"id"`
	Scope     HoldScope `json:"scope"`
	Reference string    `json:"reference"`
	Status    string    `json:"status"`
	ReviewOn  string    `json:"review_on"`
}

// HospitalAdmin is an active hospital admin: who to contact.
type HospitalAdmin struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Hospital is a hospital's page.
type Hospital struct {
	HospitalSummary
	Country      string          `json:"country"`
	TimeZone     string          `json:"time_zone"`
	EmailDomains []string        `json:"email_domains"`
	Version      int             `json:"version"`
	Deactivation *struct{}       `json:"deactivation"`
	FirstAdmin   *struct{}       `json:"first_admin"`
	Removal      *struct{}       `json:"removal"`
	Admins       []HospitalAdmin `json:"admins"`
	LegalHolds   []HoldShort     `json:"legal_holds"`
}

// HospitalsOut is the list's page.
type HospitalsOut struct {
	Hospitals  []HospitalSummary `json:"hospitals"`
	NextCursor *string           `json:"next_cursor"`
}

// hospitalCursor is the last row a page showed.
type hospitalCursor struct {
	Name string `json:"n"`
	ID   string `json:"i"`
}

func encodeHospitalCursor(name string, id uuid.UUID) string {
	b, _ := json.Marshal(hospitalCursor{Name: name, ID: id.String()})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeHospitalCursor(s string) (hospitalCursor, bool) {
	var c hospitalCursor
	if len(s) > hospitalCursorMax {
		return c, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return c, false
	}
	return c, canonicalUUID(c.ID) && utf8.ValidString(c.Name)
}

// hospitalQueryOK reports whether q is a search: 1 to 100 characters, valid
// UTF-8, no control characters.
func hospitalQueryOK(q string) bool {
	if q == "" || !utf8.ValidString(q) || utf8.RuneCountInString(q) > hospitalQueryMax {
		return false
	}
	return !strings.ContainsFunc(q, unicode.IsControl)
}

// summarySelect reads a hospital's summary; the staff count, last activity
// and open holds come from mhai_platform's reads (users, sessions,
// legal_holds).
const summarySelect = `SELECT h.id, h.name, h.short_code, h.status, h.created_at, h.time_zone,
		(SELECT count(*) FROM users u WHERE u.hospital_id = h.id AND u.is_active),
		(SELECT max(s.last_seen_at) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.hospital_id = h.id),
		(SELECT count(*) FROM legal_holds l WHERE l.hospital_id = h.id AND l.lifted_at IS NULL)
	FROM hospitals h`

func scanSummary(row pgx.Row) (HospitalSummary, string, error) {
	var s HospitalSummary
	var id uuid.UUID
	var created time.Time
	var last *time.Time
	var tz string
	if err := row.Scan(&id, &s.Name, &s.ShortCode, &s.Status, &created, &tz, &s.StaffCount, &last, &s.LegalHoldsActive); err != nil {
		return s, "", err
	}
	s.ID, s.CreatedAt = id.String(), stamp(created)
	if last != nil {
		l := stamp(*last)
		s.LastActivityAt = &l
	}
	return s, tz, nil
}

// hospitalList is GET /platform/hospitals.
func (d Deps) hospitalList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"status", "q", "cursor"})
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var status, q *string
	if s, ok := v["status"]; ok {
		if !slices.Contains(HospitalStatuses, s) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		status = &s
	}
	if s, ok := v["q"]; ok {
		if !hospitalQueryOK(s) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		q = &s
	}
	var after *hospitalCursor
	if s, ok := v["cursor"]; ok {
		c, ok := decodeHospitalCursor(s)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		after = &c
	}
	var afterName, afterID any
	if after != nil {
		afterName, afterID = after.Name, after.ID
	}
	out := HospitalsOut{Hospitals: []HospitalSummary{}}
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		// strpos, not LIKE: the search is plain text, wildcards included.
		rows, err := tx.Query(r.Context(), summarySelect+`
			WHERE ($1::text IS NULL OR h.status = $1)
			  AND ($2::text IS NULL OR strpos(lower(h.name), lower($2)) > 0 OR strpos(lower(h.short_code), lower($2)) > 0)
			  AND ($3::text IS NULL OR (h.name, h.id) > ($3::text, $4::uuid))
			ORDER BY h.name, h.id LIMIT $5`, status, q, afterName, afterID, HospitalsPage+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s, _, err := scanSummary(rows)
			if err != nil {
				return err
			}
			out.Hospitals = append(out.Hospitals, s)
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "hospitals list")
		return
	}
	if len(out.Hospitals) > HospitalsPage {
		out.Hospitals = out.Hospitals[:HospitalsPage]
		last := out.Hospitals[HospitalsPage-1]
		c := encodeHospitalCursor(last.Name, uuid.MustParse(last.ID))
		out.NextCursor = &c
	}
	httpx.JSON(w, http.StatusOK, out)
}

// errNoHospital: no hospital has the id.
var errNoHospital = errors.New("platform: no such hospital")

// openHolds are hid's open holds, newest first, in short form. today is the
// hospital's day.
func openHolds(ctx context.Context, tx pgx.Tx, hid uuid.UUID, tz string) ([]HoldShort, error) {
	rows, err := tx.Query(ctx, `SELECT id, reference,
			((placed_at AT TIME ZONE $2) + make_interval(months => $3))::date,
			(now() AT TIME ZONE $2)::date
		FROM legal_holds WHERE hospital_id = $1 AND lifted_at IS NULL ORDER BY placed_at DESC, id`, hid, tz, HoldReviewMonths)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HoldShort{}
	for rows.Next() {
		var id uuid.UUID
		var h HoldShort
		var review, today time.Time
		if err := rows.Scan(&id, &h.Reference, &review, &today); err != nil {
			return nil, err
		}
		h.ID, h.ReviewOn, h.Status = id.String(), review.Format(time.DateOnly), "active"
		if !today.Before(review) {
			h.Status = "review_due"
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// readHospital is hid's page, or errNoHospital.
func readHospital(ctx context.Context, tx pgx.Tx, hid uuid.UUID) (Hospital, error) {
	out := Hospital{EmailDomains: []string{}, Version: 1, Admins: []HospitalAdmin{}}
	s, tz, err := scanSummary(tx.QueryRow(ctx, summarySelect+` WHERE h.id = $1`, hid))
	if errors.Is(err, pgx.ErrNoRows) {
		return out, errNoHospital
	}
	if err != nil {
		return out, err
	}
	out.HospitalSummary, out.TimeZone = s, tz
	rows, err := tx.Query(ctx, `SELECT u.display_name, u.email::text FROM users u
		WHERE u.hospital_id = $1 AND u.is_active
		  AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'hospital_admin')
		ORDER BY u.display_name, u.id`, hid)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var a HospitalAdmin
		if err := rows.Scan(&a.Name, &a.Email); err != nil {
			rows.Close()
			return out, err
		}
		out.Admins = append(out.Admins, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	out.LegalHolds, err = openHolds(ctx, tx, hid, tz)
	return out, err
}

// pagePath is the path's hospital for a read that takes no query: false
// once it has answered (404 or 422).
func pagePath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	hid, ok := pathHospital(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return uuid.Nil, false
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return uuid.Nil, false
	}
	return hid, true
}

// hospitalGet is GET /platform/hospitals/{id}.
func (d Deps) hospitalGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pagePath(w, r)
	if !ok {
		return
	}
	var out Hospital
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = readHospital(r.Context(), tx, hid)
		return err
	})
	switch {
	case errors.Is(err, errNoHospital):
		httpx.Error(w, r, "not_found")
	case err != nil:
		d.fail(w, r, p, "hospital read")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}

// SSOOut is a hospital's single sign-on as the platform app reads it.
type SSOOut struct {
	Enabled        bool      `json:"enabled"`
	SignInURL      *string   `json:"sign_in_url"`
	ClientID       *string   `json:"client_id"`
	Issuer         *string   `json:"issuer"`
	AllowedDomains []string  `json:"allowed_domains"`
	SecretSaved    bool      `json:"secret_saved"`
	SecretEnds     *string   `json:"secret_ends"`
	LastTest       *struct{} `json:"last_test"`
	Version        int       `json:"version"`
}

// hospitalSSO is GET /platform/hospitals/{id}/sso: off, for a hospital
// that exists.
func (d Deps) hospitalSSO(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pagePath(w, r)
	if !ok {
		return
	}
	var exists bool
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		exists, err = hospitalExists(r.Context(), tx, hid)
		return err
	}); err != nil {
		d.fail(w, r, p, "hospital sso read")
		return
	}
	if !exists {
		httpx.Error(w, r, "not_found")
		return
	}
	httpx.JSON(w, http.StatusOK, SSOOut{AllowedDomains: []string{}, Version: 1})
}
