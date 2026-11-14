// Package patients serves a clinician's patient lookup by MRN (POST
// /patients/lookup; TODO.md's design note "Clinician patient lookup by
// MRN", approved by the product owner 14 Nov 2026), so a question can name
// a patient no alert links to (POST /queries patient_id, EMR PR 13).
//
// The attacker in mind: a clinician (or a stolen clinical session)
// browsing patients who aren't theirs, or walking sequential MRNs to
// enumerate the patient list; another hospital's clinician probing for a
// patient. So:
//
//   - The MRN goes in the body, never in a URL (a query string is 422), and
//     the route keeps no idempotency row: no hash of the MRN, no reply.
//   - Clinicians only, on a two-step session, and only while EMR is on
//     (CORE_EMR_ENABLED; off, every lookup is 404 not_found and the
//     database isn't asked).
//   - Exact match only: the MRN as typed is normalised as the EMR index
//     stores it (spaces removed, upper case; services/emr index.NormaliseMRN)
//     and compared whole, with =, against the caller's hospital's active
//     patients. No prefix, substring, pattern or name search, no list.
//   - The reply is one patient (id, name, MRN, age, sex) or 404 not_found:
//     a miss, an excluded or merged patient and another hospital's patient
//     are the same 404. Two active patients with one MRN are 409 conflict,
//     never a list to choose from.
//   - Rate limits that count misses as well as hits, per clinician a
//     minute and a hospital day and per hospital a day: each lookup takes
//     its slot first, in its own committed transaction (migration 00039,
//     mhai_patient_lookup_reserve), so parallel lookups can't overrun them.
//     Past one, 429 rate_limited, and the clinician's first refusal of the
//     day writes one patients.lookup_limit_reached entry.
//   - Every lookup is audited (patients.lookup): the patient's id on a
//     hit, both ids for an MRN of two, else that it missed; never the MRN
//     typed or a name. Logs carry ids and fixed reasons only.
//
// Identity, hospital and roles come from the verified principal only;
// every read runs as the caller's hospital and user (authz.TxUser), so
// row-level security keeps it to the caller's hospital as well.
package patients

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Limits are the lookup's rate limits (CORE_PATIENT_LOOKUP_PER_MINUTE,
// CORE_PATIENT_LOOKUP_PER_DAY, CORE_PATIENT_LOOKUP_HOSPITAL_PER_DAY):
// per clinician in any minute and in a hospital day, and per hospital in
// a day. Misses count as well as hits.
type Limits struct {
	PerMinute, PerDay, HospitalPerDay int
}

// valid reports whether l is within the bounds 00039's function takes.
func (l Limits) valid() bool {
	return l.PerMinute >= 1 && l.PerMinute <= 100 && l.PerDay >= 1 && l.PerDay <= 1000 &&
		l.HospitalPerDay >= 1 && l.HospitalPerDay <= 100000
}

// Deps are what the route needs.
type Deps struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// On is CORE_EMR_ENABLED. False: every lookup is 404 not_found and
	// the database isn't asked.
	On bool
	// Limits outside their bounds answer every lookup 503 not_ready.
	Limits Limits
	// Now is the clock (ages); defaults to time.Now.
	Now func() time.Time
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Register adds the route. NoIdempotency keeps neither a hash of the body
// (the MRN) nor the reply (a name and an MRN); NoStore says the same to
// the store, should the rule ever change.
func Register(m *authz.Mux, d Deps) {
	authz.Route(m, http.MethodPost, "/patients/lookup",
		authz.Rule{AnyRole: []string{authz.RoleClinician}, NeedsMFA: true, Write: true, NoIdempotency: true},
		httpx.NoStore(http.HandlerFunc(d.lookup)))
}

// maxTyped is the longest MRN accepted as typed, in bytes, before its
// spaces are removed.
const maxTyped = 256

// NormaliseMRN is mrn as the EMR index stores and compares it (services/emr
// index.NormaliseMRN: every space removed, upper case, nothing else), and
// whether it can be one: 1 to 64 characters, all printable. Core can't
// import the emr service (ADR 0014); TestNormaliseMRN pins the same rule.
func NormaliseMRN(mrn string) (string, bool) {
	if len(mrn) > maxTyped || !utf8.ValidString(mrn) {
		return "", false
	}
	n := strings.ToUpper(strings.Join(strings.Fields(mrn), ""))
	if n == "" || utf8.RuneCountInString(n) > 64 {
		return "", false
	}
	for _, r := range n {
		if !unicode.IsPrint(r) {
			return "", false
		}
	}
	return n, true
}

// lookupIn is the body: exactly {mrn}.
type lookupIn struct {
	MRN *string `json:"mrn"`
}

// PatientOut is the patient found: the MHAI id (lower case), to send as
// patient_id on POST /queries, the name (null if the record has none), the
// MRN as stored, the age in whole years (null without a birth date) and
// the sex.
type PatientOut struct {
	ID       string  `json:"id"`
	FullName *string `json:"full_name"`
	MRN      string  `json:"mrn"`
	Age      *int    `json:"age"`
	Sex      string  `json:"sex"`
}

// The closed outcomes the handler maps to codes.
var (
	errUnusable  = errors.New("patients: account or hospital not usable")
	errNotLooker = errors.New("patients: not a clinician with two-step")
)

// lookup is POST /patients/lookup {mrn}. Order: the principal; the body
// and the MRN (422; nothing counted); EMR off (404, the database not
// asked); a slot reserved and committed (429 past a limit; 503 when it
// can't be); then, in one transaction as the caller, the account, the
// hospital and the role re-read (401, 403), the exact match (200, 404,
// 409) and its audit entry.
func (d Deps) lookup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, ok := authz.FromContext(ctx)
	if !ok || p.Platform || p.HospitalID == uuid.Nil {
		httpx.Error(w, r, "internal_error")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var in lookupIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.MRN == nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	mrn, ok := NormaliseMRN(*in.MRN)
	in.MRN = nil
	if !ok {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.On {
		httpx.Error(w, r, "not_found")
		return
	}
	if d.Pool == nil || !d.Limits.valid() {
		httpx.Error(w, r, "not_ready")
		return
	}
	rid := httpx.RequestIDFrom(ctx)
	admit, err := d.reserve(ctx, p)
	if err != nil {
		// Fail closed: a limit that can't be counted refuses the lookup.
		d.log().Error("patients: lookup reservation failed", "request_id", rid, "hospital_id", p.HospitalID.String(),
			"reason", "unavailable")
		httpx.Error(w, r, "not_ready")
		return
	}
	switch admit {
	case "ok":
	case "minute":
		httpx.ErrorRetryAfter(w, r, "rate_limited", 60)
		return
	default:
		// The day's limits lift at the hospital's midnight.
		httpx.Error(w, r, "rate_limited")
		return
	}
	var found []PatientOut
	err = authz.TxUser(ctx, d.Pool, func(tx pgx.Tx) error {
		if err := stillAllowed(ctx, tx, p); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT p.id, p.display_name, p.mrn, p.birth_date, p.sex FROM emr.patients p
			WHERE p.hospital_id = $1 AND p.mrn = $2 AND p.status = 'active' ORDER BY p.id LIMIT 2`, p.HospitalID, mrn)
		if err != nil {
			return err
		}
		now := d.now()
		found, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (PatientOut, error) {
			var o PatientOut
			var id uuid.UUID
			var birth *time.Time
			if err := row.Scan(&id, &o.FullName, &o.MRN, &birth, &o.Sex); err != nil {
				return o, err
			}
			o.ID = id.String()
			if birth != nil {
				age := ageAt(*birth, now)
				o.Age = &age
			}
			return o, nil
		})
		if err != nil {
			return err
		}
		// Ids and codes only: the patient(s) found, else that it missed.
		e := audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(), RequestID: rid,
			Action: "patients.lookup"}
		switch len(found) {
		case 1:
			e.Entity, e.EntityID, e.Payload = "patient", found[0].ID, map[string]any{"patient_id": found[0].ID}
		case 0:
			e.Payload = map[string]any{"result": "miss"}
		default:
			// The 409 tells the caller the MRN exists: the entry says whose.
			e.Payload = map[string]any{"patient_ids": []any{found[0].ID, found[1].ID}}
		}
		return audit.Write(ctx, tx, e)
	})
	switch {
	case errors.Is(err, errUnusable):
		httpx.Error(w, r, "unauthenticated")
	case errors.Is(err, errNotLooker):
		httpx.Error(w, r, "forbidden")
	case err != nil:
		d.log().Error("patients: lookup failed", "request_id", rid, "hospital_id", p.HospitalID.String(), "reason", "unavailable")
		httpx.Error(w, r, "internal_error")
	case len(found) == 0:
		httpx.Error(w, r, "not_found")
	case len(found) > 1:
		httpx.Error(w, r, "conflict")
	default:
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, found[0])
	}
}

// reserve takes one lookup for the caller in its own transaction, which
// commits before the lookup (reserve, then check): 'ok', or the limit that
// refuses ('minute', 'day', 'hospital_day'). The first refusal of the
// clinician's hospital day writes the one flagged entry in the same
// transaction. The hospital's time zone is read as the caller, in the same
// statement (none visible: the function refuses, and so does the lookup).
func (d Deps) reserve(ctx context.Context, p authz.Principal) (string, error) {
	var admit string
	err := authz.TxUser(ctx, d.Pool, func(tx pgx.Tx) error {
		var first bool
		if err := tx.QueryRow(ctx, `SELECT admit, first_refusal FROM mhai_patient_lookup_reserve($2, $3, $4,
			(SELECT h.time_zone FROM hospitals h WHERE h.id = $1))`,
			p.HospitalID, d.Limits.PerMinute, d.Limits.PerDay, d.Limits.HospitalPerDay).Scan(&admit, &first); err != nil {
			return err
		}
		if !first {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "patients.lookup_limit_reached", Entity: "user",
			EntityID: p.UserID.String(), Payload: map[string]any{"window": admit}})
	})
	return admit, err
}

// stillAllowed re-reads, in the lookup's transaction, what the guard's
// cache may hold for up to 30 seconds: the account active and in this
// hospital, the hospital active (FOR SHARE: a deactivation waits for the
// lookup), and the clinician role and two-step still held.
func stillAllowed(ctx context.Context, tx pgx.Tx, p authz.Principal) error {
	var usable, clinician bool
	err := tx.QueryRow(ctx, `SELECT u.is_active AND h.status = 'active',
		COALESCE(m.enabled, false) AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = $3)
		FROM users u JOIN hospitals h ON h.id = u.hospital_id LEFT JOIN user_mfa m ON m.user_id = u.id
		WHERE u.id = $1 AND u.hospital_id = $2 FOR SHARE OF u`, p.UserID, p.HospitalID, authz.RoleClinician).Scan(&usable, &clinician)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errUnusable
	case err != nil:
		return err
	case !usable:
		return errUnusable
	case !clinician:
		return errNotLooker
	}
	return nil
}

// ageAt is the whole years from birth to now (as the alerts show it).
func ageAt(birth, now time.Time) int {
	y := now.Year() - birth.Year()
	if now.Month() < birth.Month() || (now.Month() == birth.Month() && now.Day() < birth.Day()) {
		y--
	}
	return max(y, 0)
}
