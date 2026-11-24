package platform

// Editing a hospital's details from the platform app (mhai-web's Edit
// hospital form, HospitalPatch; migration 00059):
//
//   PATCH /platform/hospitals/{id} {version, name?, short_code?, country?,
//   time_zone?, email_domains?} → 200 and the hospital's page.
//
// Platform admins only (auditors 403), on a two-step session; no step-up
// (the form sends none). Idempotency-Key as every platform write. Each
// field left out is kept; one sent is checked exactly as POST
// /platform/hospitals checks it (422 validation_failed, null included).
// version is the hospital's as the page read it: another is 409 conflict.
// An unknown id is 404, a removed hospital 409 hospital_removed, a short
// code another hospital has 409 duplicate. When something changes, the
// version goes up by one and hospital.updated {hospital_id, fields, via:
// platform} names the fields that changed (never their values); an edit
// that changes nothing writes nothing.

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/account"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// HospitalEditsPerMinute bounds a session's edits.
	HospitalEditsPerMinute = 10
	hospitalEditScope      = "platform.hospital.edit_rate"
)

// HospitalEditFields are the details an edit may change, in the order
// hospital.updated lists them.
var HospitalEditFields = []string{"name", "short_code", "country", "time_zone", "email_domains"}

// optStrings is a list field that tells absent from null.
type optStrings struct {
	set bool
	v   *[]string
}

func (o *optStrings) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "null" {
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	o.v = &l
	return nil
}

// hospitalPatchIn is the PATCH's body.
type hospitalPatchIn struct {
	Version      *int       `json:"version"`
	Name         optString  `json:"name"`
	ShortCode    optString  `json:"short_code"`
	Country      optString  `json:"country"`
	TimeZone     optString  `json:"time_zone"`
	EmailDomains optStrings `json:"email_domains"`
}

// hospitalDetails are the five details as stored.
type hospitalDetails struct {
	name, code, country, timeZone string
	domains                       []string
}

// apply checks in's fields against the rules of an add and returns cur
// with them applied; ok is false for any field sent but not allowed.
func (in hospitalPatchIn) apply(cur hospitalDetails) (hospitalDetails, bool) {
	next := cur
	sent := func(o optString) (string, bool, bool) {
		if !o.set {
			return "", false, true
		}
		if o.v == nil {
			return "", true, false
		}
		return *o.v, true, true
	}
	if s, set, ok := sent(in.Name); !ok {
		return next, false
	} else if set {
		if next.name, ok = account.NormaliseHospitalName(s); !ok {
			return next, false
		}
	}
	if s, set, ok := sent(in.ShortCode); !ok {
		return next, false
	} else if set {
		if next.code, ok = account.NormaliseShortCode(s); !ok {
			return next, false
		}
	}
	if s, set, ok := sent(in.Country); !ok {
		return next, false
	} else if set {
		if next.country, ok = NormaliseCountry(s); !ok {
			return next, false
		}
	}
	if s, set, ok := sent(in.TimeZone); !ok {
		return next, false
	} else if set {
		if !account.ValidTimeZone(s) {
			return next, false
		}
		next.timeZone = s
	}
	if in.EmailDomains.set {
		if in.EmailDomains.v == nil {
			return next, false
		}
		var ok bool
		if next.domains, ok = NormaliseEmailDomains(*in.EmailDomains.v); !ok {
			return next, false
		}
	}
	return next, true
}

// changedFields are the details that differ between a and b.
func changedFields(a, b hospitalDetails) []string {
	var out []string
	for _, f := range HospitalEditFields {
		var same bool
		switch f {
		case "name":
			same = a.name == b.name
		case "short_code":
			same = a.code == b.code
		case "country":
			same = a.country == b.country
		case "time_zone":
			same = a.timeZone == b.timeZone
		case "email_domains":
			same = slices.Equal(a.domains, b.domains)
		}
		if !same {
			out = append(out, f)
		}
	}
	return out
}

var (
	errHospitalRemoved = errors.New("platform: hospital removed")
	errStaleVersion    = errors.New("platform: stale version")
	errBadEdit         = errors.New("platform: bad edit")
)

// editHospital applies in to hid in tx and reads the page back.
func editHospital(tx pgx.Tx, r *http.Request, hid, actor uuid.UUID, in hospitalPatchIn) (Hospital, error) {
	ctx := r.Context()
	var cur hospitalDetails
	var status string
	var version int
	err := tx.QueryRow(ctx, `SELECT name, short_code, COALESCE(country, ''), time_zone, email_domains, status, version
		FROM hospitals WHERE id = $1 FOR UPDATE`, hid).
		Scan(&cur.name, &cur.code, &cur.country, &cur.timeZone, &cur.domains, &status, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		// FOR UPDATE sees only rows 00059's edit policy lets it change: a
		// removed hospital is visible to a plain read only.
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM hospitals WHERE id = $1)`, hid).Scan(&exists); err != nil {
			return Hospital{}, err
		}
		if exists {
			return Hospital{}, errHospitalRemoved
		}
		return Hospital{}, errNoHospital
	}
	if err != nil {
		return Hospital{}, err
	}
	if status == "removed" {
		return Hospital{}, errHospitalRemoved
	}
	if *in.Version != version {
		return Hospital{}, errStaleVersion
	}
	next, ok := in.apply(cur)
	if !ok {
		return Hospital{}, errBadEdit
	}
	if fields := changedFields(cur, next); len(fields) > 0 {
		var country any
		if next.country != "" {
			country = next.country
		}
		if next.domains == nil {
			next.domains = []string{}
		}
		if _, err := tx.Exec(ctx, `UPDATE hospitals SET name = $2, short_code = $3, country = $4, time_zone = $5,
			email_domains = $6, version = version + 1 WHERE id = $1`,
			hid, next.name, next.code, country, next.timeZone, next.domains); err != nil {
			if uniqueOn(err, "hospitals_short_code_key") {
				return Hospital{}, errDuplicateCode
			}
			return Hospital{}, err
		}
		// Field names only: never a name, code or domain.
		if err := audit.Write(ctx, tx, audit.Event{ActorID: actor.String(), RequestID: httpx.RequestIDFrom(ctx),
			Action: "hospital.updated", Entity: "hospital", EntityID: hid.String(),
			Payload: map[string]any{"hospital_id": hid.String(), "fields": fields, "via": ViaPlatform}}); err != nil {
			return Hospital{}, err
		}
	}
	return readHospital(ctx, tx, hid)
}

// hospitalEdit is PATCH /platform/hospitals/{id}. Order: the path (404),
// the query and the body's shape (422), the session's minute (429), then
// in one transaction the hospital (404), removed (409 hospital_removed),
// the version (409 conflict), the fields (422), the short code (409
// duplicate).
func (d Deps) hospitalEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	hid, ok := pathHospital(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in hospitalPatchIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	// The fields are checked against nothing here, so a bad one is 422
	// before the hospital is looked up.
	if in.Version == nil || *in.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if _, ok := in.apply(hospitalDetails{}); !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	allowed, err := d.reserveRate(r, p, hospitalEditScope, "platform.hospital.edit.v1", HospitalEditsPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: hospital edit refused: rate counter unavailable", "request_id", rid, "reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return
	}
	var out Hospital
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = editHospital(tx, r, hid, p.UserID, in)
		return err
	})
	switch {
	case errors.Is(err, errNoHospital):
		httpx.Error(w, r, "not_found")
	case errors.Is(err, errHospitalRemoved):
		httpx.Error(w, r, "hospital_removed")
	case errors.Is(err, errStaleVersion):
		httpx.Error(w, r, "conflict")
	case errors.Is(err, errBadEdit):
		httpx.Error(w, r, "validation_failed")
	case errors.Is(err, errDuplicateCode):
		httpx.Error(w, r, "duplicate")
	case err != nil:
		d.log().Error("platform: hospital edit failed", "request_id", rid, "user_id", p.UserID.String(), "reason", "internal_error")
		httpx.Error(w, r, "internal_error")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}
