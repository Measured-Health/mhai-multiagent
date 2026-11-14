package hospital

// GET and PATCH /hospital/settings (frontend requirements §5.2). A PATCH is
// made at the version it was read at (409 conflict otherwise), saves only
// the choices Measured Health allows (Limits), and is audited with the
// field names only. One that loosens a sign-in protection (two-step for
// clinicians off, trusted devices on, a longer idle timeout) also needs a
// fresh step-up code; tightening never does.
//
// Read-only here, and with no feature behind them in core yet: SSO
// (always not_set_up), allowed email domains (always empty) and
// reviewers_share_specialty (always false: every clinician reviews every
// correction; a PATCH setting it true is 422).

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

// LimitsOut are the choices Measured Health allows.
type LimitsOut struct {
	ReviewOverdueHours []int `json:"review_overdue_hours"`
	IdleTimeoutMinutes []int `json:"idle_timeout_minutes"`
}

// Limits are the allowed choices.
var Limits = LimitsOut{
	ReviewOverdueHours: []int{1, 2, 4, 8, 24},
	IdleTimeoutMinutes: []int{5, 10, 15, 30},
}

// SSOOut is the hospital's single sign-on, read-only.
type SSOOut struct {
	Status string  `json:"status"`
	Domain *string `json:"domain"`
}

// SettingsOut is HospitalSettingsOut.
type SettingsOut struct {
	HospitalName            string    `json:"hospital_name"`
	ReviewOverdueHours      int       `json:"review_overdue_hours"`
	ReviewersShareSpecialty bool      `json:"reviewers_share_specialty"`
	RequireMFAForClinicians bool      `json:"require_mfa_for_clinicians"`
	IdleTimeoutMinutes      int       `json:"idle_timeout_minutes"`
	AllowTrustDevice        bool      `json:"allow_trust_device"`
	SSO                     SSOOut    `json:"sso"`
	AllowedEmailDomains     []string  `json:"allowed_email_domains"`
	Specialties             []string  `json:"specialties"`
	Limits                  LimitsOut `json:"limits"`
	Version                 int       `json:"version"`

	stored bool
}

// maxSpecialties bounds the specialties list.
const maxSpecialties = 200

func loadSettings(ctx context.Context, tx pgx.Tx, hid uuid.UUID, lock bool) (SettingsOut, error) {
	out := SettingsOut{ReviewOverdueHours: 4, IdleTimeoutMinutes: 15, RequireMFAForClinicians: true,
		SSO: SSOOut{Status: "not_set_up"}, AllowedEmailDomains: []string{}, Specialties: []string{}, Limits: Limits, Version: 1}
	if err := tx.QueryRow(ctx, `SELECT name FROM hospitals WHERE id = $1`, hid).Scan(&out.HospitalName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, errNotFound
		}
		return out, err
	}
	q := `SELECT review_overdue_hours, require_mfa_for_clinicians, idle_timeout_minutes, allow_trust_device, version
		FROM hospital_settings WHERE hospital_id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, q, hid).Scan(&out.ReviewOverdueHours, &out.RequireMFAForClinicians, &out.IdleTimeoutMinutes,
		&out.AllowTrustDevice, &out.Version)
	switch {
	case err == nil:
		out.stored = true
	case !errors.Is(err, pgx.ErrNoRows):
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT specialty FROM users WHERE hospital_id = $1 AND is_active AND specialty IS NOT NULL
		ORDER BY specialty LIMIT $2`, hid, maxSpecialties)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return out, err
		}
		out.Specialties = append(out.Specialties, s)
	}
	return out, rows.Err()
}

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
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = loadSettings(r.Context(), tx, p.HospitalID, false)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "settings read", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

type settingsIn struct {
	Version                 *int  `json:"version"`
	ReviewOverdueHours      *int  `json:"review_overdue_hours"`
	ReviewersShareSpecialty *bool `json:"reviewers_share_specialty"`
	RequireMFAForClinicians *bool `json:"require_mfa_for_clinicians"`
	IdleTimeoutMinutes      *int  `json:"idle_timeout_minutes"`
	AllowTrustDevice        *bool `json:"allow_trust_device"`
	stepUpIn
}

func validSettings(in settingsIn) error {
	if in.Version == nil || *in.Version < 1 {
		return errInvalid
	}
	if in.ReviewOverdueHours != nil && !slices.Contains(Limits.ReviewOverdueHours, *in.ReviewOverdueHours) {
		return errInvalid
	}
	if in.IdleTimeoutMinutes != nil && !slices.Contains(Limits.IdleTimeoutMinutes, *in.IdleTimeoutMinutes) {
		return errInvalid
	}
	if in.ReviewersShareSpecialty != nil && *in.ReviewersShareSpecialty {
		return errInvalid
	}
	return nil
}

// settingsChange is what a PATCH changes against cur: the field names
// (closed set, in a fixed order) and whether any loosens sign-in.
func settingsChange(in settingsIn, cur SettingsOut) (fields []string, loosened bool) {
	fields = []string{}
	if in.ReviewOverdueHours != nil && *in.ReviewOverdueHours != cur.ReviewOverdueHours {
		fields = append(fields, "review_overdue_hours")
	}
	if in.RequireMFAForClinicians != nil && *in.RequireMFAForClinicians != cur.RequireMFAForClinicians {
		fields = append(fields, "require_mfa_for_clinicians")
		loosened = loosened || !*in.RequireMFAForClinicians
	}
	if in.IdleTimeoutMinutes != nil && *in.IdleTimeoutMinutes != cur.IdleTimeoutMinutes {
		fields = append(fields, "idle_timeout_minutes")
		loosened = loosened || *in.IdleTimeoutMinutes > cur.IdleTimeoutMinutes
	}
	if in.AllowTrustDevice != nil && *in.AllowTrustDevice != cur.AllowTrustDevice {
		fields = append(fields, "allow_trust_device")
		loosened = loosened || *in.AllowTrustDevice
	}
	return fields, loosened
}

// settingsPatch is PATCH /hospital/settings {version, ..., mfa_code |
// backup_code when loosening}.
func (d Deps) settingsPatch(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in settingsIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if err := validSettings(in); err != nil {
		d.fail(w, r, p, "settings edit", err)
		return
	}
	ctx := r.Context()
	var cur SettingsOut
	if err := d.tx(ctx, func(tx pgx.Tx) error {
		var err error
		cur, err = loadSettings(ctx, tx, p.HospitalID, false)
		return err
	}); err != nil {
		d.fail(w, r, p, "settings edit", err)
		return
	}
	if *in.Version != cur.Version || !cur.stored {
		d.fail(w, r, p, "settings edit", errConflict)
		return
	}
	fields, loosened := settingsChange(in, cur)
	if len(fields) == 0 {
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, cur)
		return
	}
	if loosened && !d.stepUp(w, r, in.stepUpIn) {
		return
	}
	var out SettingsOut
	err := d.tx(ctx, func(tx pgx.Tx) error {
		now, err := loadSettings(ctx, tx, p.HospitalID, true)
		if err != nil {
			return err
		}
		if now.Version != *in.Version {
			return errConflict
		}
		// What the code was checked for is what is saved: a change that
		// became a loosening since the first read is refused.
		f2, l2 := settingsChange(in, now)
		if l2 && !loosened {
			return errConflict
		}
		ct, err := tx.Exec(ctx, `UPDATE hospital_settings SET
			review_overdue_hours = COALESCE($2, review_overdue_hours),
			require_mfa_for_clinicians = COALESCE($3, require_mfa_for_clinicians),
			idle_timeout_minutes = COALESCE($4, idle_timeout_minutes),
			allow_trust_device = COALESCE($5, allow_trust_device),
			version = version + 1, updated_by = $6, updated_at = $7
			WHERE hospital_id = $1 AND version = $8`,
			p.HospitalID, in.ReviewOverdueHours, in.RequireMFAForClinicians, in.IdleTimeoutMinutes, in.AllowTrustDevice,
			p.UserID, d.now().UTC().Truncate(time.Microsecond), *in.Version)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			return errConflict
		}
		if err := audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "hospital_settings.changed", Entity: "hospital",
			EntityID: p.HospitalID.String(), Payload: map[string]any{"fields": f2, "loosened": l2}}); err != nil {
			return err
		}
		out, err = loadSettings(ctx, tx, p.HospitalID, false)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "settings edit", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}
