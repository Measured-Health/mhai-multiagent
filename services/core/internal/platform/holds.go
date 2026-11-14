package platform

// Legal holds in the platform app (mhai-web Plan 8a; the holds core keeps
// since EMR PR 9, 00032, extended by 00037):
//
//   - GET /platform/legal-holds?status&hospital&q&cursor: platform admins
//     and auditors; newest first, 50 a page; status open (the default:
//     active, review due, lift requested), active, review_due,
//     lift_requested, lifted or all; q a part of the reference, any case.
//   - GET /platform/legal-holds/{id}: the hold and its history (its audit
//     rows, newest first).
//   - POST /platform/legal-holds {hospital_id, reason, reference,
//     review_on, mfa_code | backup_code}: platform admins, with step-up.
//   - POST /platform/legal-holds/{id}/lift {reason, authority, code}: the
//     first of two platform admins asks, with step-up; the request lapses
//     after 7 days.
//   - POST /platform/legal-holds/{id}/lift/approve {version, code}: a
//     different platform admin, with step-up, lifts the hold (409
//     same_person for the requester, 409 expired once it lapsed, 409
//     conflict on a stale version or when the requester is no longer an
//     active platform admin). Removing or demoting a platform admin
//     cancels their open requests (team.go).
//   - POST /platform/legal-holds/{id}/lift/cancel {version}: either, no
//     step-up.
//   - PATCH /platform/legal-holds/{id} and POST .../lift/undo: 501.
//
// Where core's model is stricter than mhai-web's contract, core's wins
// (recorded for the frontend in the PR): a hold keeps every record (a
// from or to date is 422 invalid_range); a hold carries no free text and
// no name, so note and asked_by are refused when sent (422) and read back
// null; a lift's authority is a document reference (as a hold's); core
// lifts at once on approval (no grace period: released_on is the
// approval's time and there is no undo); records_due and due_after are
// null (core doesn't count them). A hold placed by the provisioning CLI has
// no reason, review date or placer: reason null, the review date six
// months after placing, and placed_by the CLI marker {id "cli"}.
//
// Every write and its audit row are one transaction; the hold's row lock
// orders concurrent lifts, and the database refuses an approval by the
// requester (legal_hold_lifts_two_people), and lifts a hold only with an
// approved request of two different people (00037's platform_lift),
// whatever the handler does.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// HoldsPage is the list's page size.
	HoldsPage = 50
	// LiftLapse is how long a lift request waits for its approval.
	LiftLapse = 7 * 24 * time.Hour
	// HoldWritesPerMinute is the hold writes one session may make a minute.
	HoldWritesPerMinute = 10
	holdScope           = "platform.legal_hold.write_rate"
	// maxReviewYears bounds a review date.
	maxReviewYears = 5
	// CLIPerson is the id of "the provisioning command line" as a person.
	CLIPerson = "cli"
)

var (
	// HoldReasons and LiftReasons are 00037's codes.
	HoldReasons = []string{"court_case", "regulator", "complaint", "investigation", "other"}
	LiftReasons = []string{"case_closed", "withdrawn", "mistake", "other"}
	// holdFilters are the list's status filters.
	holdFilters = []string{"open", "active", "review_due", "lift_requested", "lifted", "all"}
	// HoldReferencePattern is legal_holds.reference's CHECK (00032), and a
	// lift's authority's (00037).
	HoldReferencePattern = regexp.MustCompile(`^[A-Z]{2,8}-[0-9]{4}-[0-9]{2,8}$`)
)

var (
	errNoHold        = errors.New("platform: no such hold")
	errHoldConflict  = errors.New("platform: hold changed")
	errHoldExpired   = errors.New("platform: lift request lapsed")
	errSamePerson    = errors.New("platform: same person")
	errHospitalGone  = errors.New("platform: hospital removed")
	errHoldReference = errors.New("platform: an open hold has the reference")
)

func registerHolds(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles}
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true}
	stepUp := write
	stepUp.StepUp = true
	authz.Route(m, http.MethodGet, "/platform/legal-holds", read, http.HandlerFunc(d.holdList))
	authz.Route(m, http.MethodPost, "/platform/legal-holds", stepUp, http.HandlerFunc(d.holdPlace))
	authz.Route(m, http.MethodGet, "/platform/legal-holds/{id}", read, http.HandlerFunc(d.holdGet))
	authz.Route(m, http.MethodPatch, "/platform/legal-holds/{id}", write, http.HandlerFunc(notImplemented))
	authz.Route(m, http.MethodPost, "/platform/legal-holds/{id}/lift", stepUp, http.HandlerFunc(d.holdLift))
	authz.Route(m, http.MethodPost, "/platform/legal-holds/{id}/lift/approve", stepUp, http.HandlerFunc(d.holdApprove))
	authz.Route(m, http.MethodPost, "/platform/legal-holds/{id}/lift/cancel", write, http.HandlerFunc(d.holdCancel))
	authz.Route(m, http.MethodPost, "/platform/legal-holds/{id}/lift/undo", stepUp, http.HandlerFunc(notImplemented))
}

// Person is someone (or the CLI) as the platform app names them.
type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// HoldLift is a hold's lift request.
type HoldLift struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	RequestedBy    Person  `json:"requested_by"`
	RequestedAt    string  `json:"requested_at"`
	Reason         string  `json:"reason"`
	RequestedByYou bool    `json:"requested_by_you"`
	ExpiresAt      string  `json:"expires_at"`
	Note           *string `json:"note"`
	Authority      string  `json:"authority"`
	RecordsDue     *int64  `json:"records_due"`
	DueAfter       *string `json:"due_after"`
	ApprovedBy     *Person `json:"approved_by"`
	ApprovedAt     *string `json:"approved_at"`
	ReleasedOn     *string `json:"released_on"`
}

// LegalHold is a hold as the list and the page show it.
type LegalHold struct {
	ID             string    `json:"id"`
	Hospital       Person    `json:"hospital"`
	Scope          HoldScope `json:"scope"`
	Reason         *string   `json:"reason"`
	Reference      string    `json:"reference"`
	AskedBy        *string   `json:"asked_by"`
	Note           *string   `json:"note"`
	PlacedBy       Person    `json:"placed_by"`
	PlacedAt       string    `json:"placed_at"`
	ReviewOn       string    `json:"review_on"`
	Status         string    `json:"status"`
	Lift           *HoldLift `json:"lift"`
	StoppedRemoval bool      `json:"stopped_removal"`
	Version        int       `json:"version"`
}

// HoldEvent is one line of a hold's history.
type HoldEvent struct {
	At     string  `json:"at"`
	Actor  Person  `json:"actor"`
	Action string  `json:"action"`
	Detail *string `json:"detail"`
}

// LegalHoldDetail is a hold with its history.
type LegalHoldDetail struct {
	LegalHold
	History []HoldEvent `json:"history"`
}

// holdSelect reads holds with their latest lift request (open, or the
// approved one of a lifted hold) and the hospital's day. $1 is the caller.
const holdSelect = `SELECT l.id, h.id, h.name, l.reason, l.reference, l.placed_by, pu.display_name, l.placed_at,
		COALESCE(l.review_on, ((l.placed_at AT TIME ZONE h.time_zone) + make_interval(months => 6))::date),
		(now() AT TIME ZONE h.time_zone)::date, l.lifted_at, l.version,
		k.id, k.reason, k.authority, k.requested_by, ku.display_name, k.requested_at, k.expires_at,
		k.approved_by, au.display_name, k.approved_at
	FROM legal_holds l
	JOIN hospitals h ON h.id = l.hospital_id
	LEFT JOIN users pu ON pu.id = l.placed_by
	LEFT JOIN LATERAL (SELECT * FROM legal_hold_lifts x
		WHERE x.hold_id = l.id AND ((x.approved_at IS NULL AND x.cancelled_at IS NULL AND x.expires_at > now())
		                            OR x.approved_at IS NOT NULL)
		ORDER BY x.requested_at DESC LIMIT 1) k ON true
	LEFT JOIN users ku ON ku.id = k.requested_by
	LEFT JOIN users au ON au.id = k.approved_by`

// holdStatus is the status expression of holdSelect's row (for filters).
const holdStatus = `CASE WHEN l.lifted_at IS NOT NULL THEN 'lifted'
		WHEN EXISTS (SELECT 1 FROM legal_hold_lifts x WHERE x.hold_id = l.id AND x.approved_at IS NULL
		             AND x.cancelled_at IS NULL AND x.expires_at > now()) THEN 'lift_requested'
		WHEN (now() AT TIME ZONE h.time_zone)::date >= COALESCE(l.review_on,
		     ((l.placed_at AT TIME ZONE h.time_zone) + make_interval(months => 6))::date) THEN 'review_due'
		ELSE 'active' END`

func person(id *uuid.UUID, name *string) Person {
	if id == nil {
		return Person{ID: CLIPerson, Name: "Provisioning command line"}
	}
	p := Person{ID: id.String()}
	if name != nil {
		p.Name = *name
	}
	return p
}

func optPerson(id *uuid.UUID, name *string) *Person {
	if id == nil {
		return nil
	}
	p := person(id, name)
	return &p
}

func scanHold(row pgx.Row, you uuid.UUID) (LegalHold, error) {
	var h LegalHold
	var id, hid uuid.UUID
	var placedBy, kid, kBy, kApprovedBy *uuid.UUID
	var placedName, kReason, kAuthority, kByName, kApprovedName *string
	var placed time.Time
	var review, today time.Time
	var lifted, kAt, kExpires, kApproved *time.Time
	if err := row.Scan(&id, &hid, &h.Hospital.Name, &h.Reason, &h.Reference, &placedBy, &placedName, &placed,
		&review, &today, &lifted, &h.Version,
		&kid, &kReason, &kAuthority, &kBy, &kByName, &kAt, &kExpires, &kApprovedBy, &kApprovedName, &kApproved); err != nil {
		return h, err
	}
	h.ID, h.Hospital.ID, h.PlacedAt = id.String(), hid.String(), stamp(placed)
	h.PlacedBy = person(placedBy, placedName)
	h.ReviewOn = review.Format(time.DateOnly)
	if kid != nil && kBy != nil && kAt != nil && kExpires != nil && kReason != nil && kAuthority != nil {
		l := &HoldLift{ID: kid.String(), Kind: "hold_lift", RequestedBy: person(kBy, kByName), RequestedAt: stamp(*kAt),
			Reason: *kReason, RequestedByYou: *kBy == you, ExpiresAt: stamp(*kExpires), Authority: *kAuthority,
			ApprovedBy: optPerson(kApprovedBy, kApprovedName)}
		if kApproved != nil {
			s := stamp(*kApproved)
			l.ApprovedAt, l.ReleasedOn = &s, &s
		}
		h.Lift = l
	}
	switch {
	case lifted != nil:
		h.Status = "lifted"
	case h.Lift != nil && h.Lift.ApprovedAt == nil:
		h.Status = "lift_requested"
	case !today.Before(review):
		h.Status = "review_due"
	default:
		h.Status = "active"
	}
	if lifted == nil && h.Lift != nil && h.Lift.ApprovedAt != nil {
		h.Lift = nil // not reachable: an approval lifts in the same transaction
	}
	return h, nil
}

// holdCursor is the last row a page showed.
type holdCursor struct {
	At string `json:"t"`
	ID string `json:"i"`
}

func decodeHoldCursor(s string) (time.Time, uuid.UUID, bool) {
	var c holdCursor
	if len(s) > hospitalCursorMax {
		return time.Time{}, uuid.Nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() || !canonicalUUID(c.ID) {
		return time.Time{}, uuid.Nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, c.At)
	if err != nil {
		return time.Time{}, uuid.Nil, false
	}
	return t, uuid.MustParse(c.ID), true
}

// holdList is GET /platform/legal-holds.
func (d Deps) holdList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"status", "hospital", "q", "cursor"})
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	status := "open"
	if s, ok := v["status"]; ok {
		if !slices.Contains(holdFilters, s) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		status = s
	}
	var hospital, q, afterID, afterAt any
	if s, ok := v["hospital"]; ok {
		if !canonicalUUID(s) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		hospital = s
	}
	if s, ok := v["q"]; ok {
		if !hospitalQueryOK(s) {
			httpx.Error(w, r, "validation_failed")
			return
		}
		q = s
	}
	if s, ok := v["cursor"]; ok {
		t, id, ok := decodeHoldCursor(s)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		afterAt, afterID = t, id
	}
	out := struct {
		Holds      []LegalHold `json:"holds"`
		NextCursor *string     `json:"next_cursor"`
	}{Holds: []LegalHold{}}
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), holdSelect+`
			WHERE ($1::uuid IS NULL OR l.hospital_id = $1)
			  AND ($2::text IS NULL OR strpos(lower(l.reference), lower($2)) > 0)
			  AND ($3::timestamptz IS NULL OR (l.placed_at, l.id) < ($3::timestamptz, $4::uuid))
			  AND CASE $5::text WHEN 'all' THEN true
			                    WHEN 'open' THEN `+holdStatus+` <> 'lifted'
			                    ELSE `+holdStatus+` = $5 END
			ORDER BY l.placed_at DESC, l.id DESC LIMIT $6`, hospital, q, afterAt, afterID, status, HoldsPage+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			h, err := scanHold(rows, p.UserID)
			if err != nil {
				return err
			}
			out.Holds = append(out.Holds, h)
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "legal holds list")
		return
	}
	if len(out.Holds) > HoldsPage {
		out.Holds = out.Holds[:HoldsPage]
		last := out.Holds[HoldsPage-1]
		// placed_at has microseconds; the stamp keeps milliseconds, so the
		// cursor carries the row's exact instant from the database.
		var exact time.Time
		if err := d.tx(r.Context(), func(tx pgx.Tx) error {
			return tx.QueryRow(r.Context(), `SELECT placed_at FROM legal_holds WHERE id = $1`, last.ID).Scan(&exact)
		}); err != nil {
			d.fail(w, r, p, "legal holds cursor")
			return
		}
		b, _ := json.Marshal(holdCursor{At: exact.UTC().Format(time.RFC3339Nano), ID: last.ID})
		c := base64.RawURLEncoding.EncodeToString(b)
		out.NextCursor = &c
	}
	httpx.JSON(w, http.StatusOK, out)
}

// pathHold is the path's hold id, or false (404).
func pathHold(r *http.Request) (uuid.UUID, bool) { return pathMember(r) }

// readHoldDetail is the hold and its history, or errNoHold.
func readHoldDetail(ctx context.Context, tx pgx.Tx, id, you uuid.UUID) (LegalHoldDetail, error) {
	var out LegalHoldDetail
	h, err := scanHold(tx.QueryRow(ctx, holdSelect+` WHERE l.id = $1`, id), you)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, errNoHold
	}
	if err != nil {
		return out, err
	}
	out.LegalHold, out.History = h, []HoldEvent{}
	rows, err := tx.Query(ctx, `SELECT a.ts, a.actor_id, u.display_name, a.action
		FROM audit_log a LEFT JOIN users u ON u.id = a.actor_id
		WHERE a.action IN ('legal_hold.placed', 'legal_hold.lift_requested', 'legal_hold.lift_cancelled', 'legal_hold.lifted')
		  AND a.payload->>'hold_id' = $1
		ORDER BY a.id DESC LIMIT 200`, id.String())
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var at time.Time
		var actor *uuid.UUID
		var name *string
		var action string
		if err := rows.Scan(&at, &actor, &name, &action); err != nil {
			return out, err
		}
		out.History = append(out.History, HoldEvent{At: stamp(at), Actor: person(actor, name),
			Action: strings.TrimPrefix(action, "legal_hold.")})
	}
	return out, rows.Err()
}

// holdGet is GET /platform/legal-holds/{id}.
func (d Deps) holdGet(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathHold(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out LegalHoldDetail
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = readHoldDetail(r.Context(), tx, id, p.UserID)
		return err
	})
	if d.holdAnswer(w, r, p, "legal hold read", err) {
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (d Deps) holdAnswer(w http.ResponseWriter, r *http.Request, p authz.Principal, what string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errNoHold), errors.Is(err, errNoHospital):
		httpx.Error(w, r, "not_found")
	case errors.Is(err, errHoldConflict), errors.Is(err, errHoldReference):
		httpx.Error(w, r, "conflict")
	case errors.Is(err, errHoldExpired):
		httpx.Error(w, r, "expired")
	case errors.Is(err, errSamePerson):
		httpx.Error(w, r, "same_person")
	case errors.Is(err, errHospitalGone):
		httpx.Error(w, r, "hospital_removed")
	default:
		d.fail(w, r, p, what)
	}
	return true
}

// holdRate counts one write against the session's minute.
func (d Deps) holdRate(w http.ResponseWriter, r *http.Request, p authz.Principal) bool {
	allowed, err := d.reserveRate(r, p, holdScope, "platform.legal_hold.write.v1", HoldWritesPerMinute)
	switch {
	case err != nil:
		d.log().Error("platform: hold write refused: rate counter unavailable", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return false
	case !allowed:
		httpx.Error(w, r, "rate_limited")
		return false
	}
	return true
}

// holdAudit appends one legal_hold.* row: no hospital on the row (a
// platform action), the hold and its hospital in the payload.
func holdAudit(ctx context.Context, tx pgx.Tx, p authz.Principal, rid, action string, hold, hospital uuid.UUID, extra map[string]any) error {
	payload := map[string]any{"hold_id": hold.String(), "hospital_id": hospital.String()}
	for k, v := range extra {
		payload[k] = v
	}
	return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), RequestID: rid, Action: action,
		Entity: "legal_hold", EntityID: hold.String(), Payload: payload})
}

// placeBody is the place's body. Note, asked_by, from and to are read only
// to refuse them.
type placeBody struct {
	HospitalID string           `json:"hospital_id"`
	From       *json.RawMessage `json:"from"`
	To         *json.RawMessage `json:"to"`
	Reason     string           `json:"reason"`
	Note       *json.RawMessage `json:"note"`
	Reference  string           `json:"reference"`
	AskedBy    *json.RawMessage `json:"asked_by"`
	ReviewOn   string           `json:"review_on"`
	MFACode    string           `json:"mfa_code"`
	BackupCode string           `json:"backup_code"`
}

// emptyOrNull reports whether a refused key is absent, null or "".
func emptyOrNull(m *json.RawMessage) bool {
	if m == nil {
		return true
	}
	s := strings.TrimSpace(string(*m))
	return s == "null" || s == `""`
}

// holdPlace is POST /platform/legal-holds. Order: the body (422; a date
// range is 422 invalid_range), the hospital (404, 409 hospital_removed),
// the session's minute (429), the code, then the write (409 conflict when
// an open hold has the reference).
func (d Deps) holdPlace(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b placeBody
	if !httpx.Decode(w, r, &b) {
		return
	}
	review, err := time.Parse(time.DateOnly, b.ReviewOn)
	today := d.now().Truncate(24 * time.Hour)
	if !canonicalUUID(b.HospitalID) || !slices.Contains(HoldReasons, b.Reason) || !HoldReferencePattern.MatchString(b.Reference) ||
		err != nil || review.Format(time.DateOnly) != b.ReviewOn || !review.After(today) ||
		review.After(today.AddDate(maxReviewYears, 0, 0)) || !emptyOrNull(b.Note) || !emptyOrNull(b.AskedBy) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !emptyOrNull(b.From) || !emptyOrNull(b.To) {
		httpx.Error(w, r, "invalid_range")
		return
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	hid := uuid.MustParse(b.HospitalID)
	if err := d.tx(r.Context(), func(tx pgx.Tx) error { return placeable(r.Context(), tx, hid, b.Reference) }); d.holdAnswer(w, r, p, "hold hospital", err) {
		return
	}
	if !d.holdRate(w, r, p) || !d.stepUp(w, r, b.MFACode, b.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var out LegalHoldDetail
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		// The hospital's hold lock first (as the insert trigger and every
		// deleting definer take it): a purge or removal either committed
		// before this reads the status, or waits for the hold.
		if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock(hashtextextended('emr-hold:' || $1::text, 0))`, hid); err != nil {
			return err
		}
		if err := placeable(r.Context(), tx, hid, b.Reference); err != nil {
			return err
		}
		var id uuid.UUID
		if err := tx.QueryRow(r.Context(), `INSERT INTO legal_holds (hospital_id, reference, reason, review_on, placed_by)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, hid, b.Reference, b.Reason, review, p.UserID).Scan(&id); err != nil {
			if pe := pgError(err); pe != nil && pe.Code == "23505" {
				return errHoldReference
			}
			return err
		}
		if err := holdAudit(r.Context(), tx, p, rid, "legal_hold.placed", id, hid, map[string]any{"reason": b.Reason}); err != nil {
			return err
		}
		var err error
		out, err = readHoldDetail(r.Context(), tx, id, p.UserID)
		return err
	})
	if d.holdAnswer(w, r, p, "legal hold place", err) {
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// holdHospital checks a hospital can take a hold: it exists (else
// errNoHospital) and isn't removed (errHospitalGone).
func holdHospital(ctx context.Context, tx pgx.Tx, hid uuid.UUID) error {
	// No row lock (mhai_platform may not lock hospitals): the place takes
	// the hospital's hold lock before it calls this.
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM hospitals WHERE id = $1`, hid).Scan(&status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return errNoHospital
	case err != nil:
		return err
	case status == "removed":
		return errHospitalGone
	}
	return nil
}

// placeable is a place's rules: the hospital can take a hold and no open
// hold has the reference.
func placeable(ctx context.Context, tx pgx.Tx, hid uuid.UUID, reference string) error {
	if err := holdHospital(ctx, tx, hid); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM legal_holds WHERE hospital_id = $1 AND reference = $2
		AND lifted_at IS NULL)`, hid, reference).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return errHoldReference
	}
	return nil
}

// lockedHold is a hold's row, locked, for a lift write.
type lockedHold struct {
	hospital uuid.UUID
	version  int
	lifted   bool
}

// A lifted hold is read without the lock: FOR UPDATE applies the update
// policy, which hides a lifted hold (it would read as 404, not 409).
func lockHold(ctx context.Context, tx pgx.Tx, id uuid.UUID) (lockedHold, error) {
	var h lockedHold
	var lifted *time.Time
	err := tx.QueryRow(ctx, `SELECT hospital_id, version, lifted_at FROM legal_holds WHERE id = $1`, id).
		Scan(&h.hospital, &h.version, &lifted)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, errNoHold
	}
	if err != nil || lifted != nil {
		h.lifted = lifted != nil
		return h, err
	}
	err = tx.QueryRow(ctx, `SELECT version, lifted_at FROM legal_holds WHERE id = $1 FOR UPDATE`, id).Scan(&h.version, &lifted)
	if errors.Is(err, pgx.ErrNoRows) {
		h.lifted = true // lifted meanwhile
		return h, nil
	}
	h.lifted = lifted != nil
	return h, err
}

// liftable is a lift request's rules: the hold open and no request
// waiting (a lapsed one is ended first when lapse is set).
func liftable(ctx context.Context, tx pgx.Tx, id uuid.UUID, lapse bool) (lockedHold, error) {
	h, err := lockHold(ctx, tx, id)
	if err != nil {
		return h, err
	}
	if h.lifted {
		return h, errHoldConflict
	}
	if lapse {
		if err := lapseLifts(ctx, tx, id); err != nil {
			return h, err
		}
	}
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM legal_hold_lifts WHERE hold_id = $1
		AND approved_at IS NULL AND cancelled_at IS NULL AND expires_at > now())`, id).Scan(&open); err != nil {
		return h, err
	}
	if open {
		return h, errHoldConflict
	}
	return h, nil
}

// lapseLifts ends the hold's lapsed open request, if any: cancelled by no
// one (the request row records the lapse; the new request's audit row
// follows it).
func lapseLifts(ctx context.Context, tx pgx.Tx, hold uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE legal_hold_lifts SET cancelled_at = now()
		WHERE hold_id = $1 AND approved_at IS NULL AND cancelled_at IS NULL AND expires_at <= now()`, hold)
	return err
}

type liftBody struct {
	Reason     string           `json:"reason"`
	Note       *json.RawMessage `json:"note"`
	Authority  string           `json:"authority"`
	MFACode    string           `json:"mfa_code"`
	BackupCode string           `json:"backup_code"`
}

// holdLift is POST /platform/legal-holds/{id}/lift.
func (d Deps) holdLift(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathHold(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	var b liftBody
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &b) {
		return
	}
	if !slices.Contains(LiftReasons, b.Reason) || !HoldReferencePattern.MatchString(b.Authority) || !emptyOrNull(b.Note) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	// The rules before a code is spent (a read-only pass), then again
	// under the hold's lock.
	if err := d.tx(r.Context(), func(tx pgx.Tx) error {
		_, err := liftable(r.Context(), tx, id, false)
		return err
	}); d.holdAnswer(w, r, p, "legal hold lookup", err) {
		return
	}
	if !d.holdRate(w, r, p) || !d.stepUp(w, r, b.MFACode, b.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var out LegalHoldDetail
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		h, err := liftable(r.Context(), tx, id, true)
		if err != nil {
			return err
		}
		var lift uuid.UUID
		if err := tx.QueryRow(r.Context(), `INSERT INTO legal_hold_lifts (hold_id, reason, authority, requested_by, expires_at)
			VALUES ($1, $2, $3, $4, now() + $5::interval) RETURNING id`, id, b.Reason, b.Authority, p.UserID,
			LiftLapse.String()).Scan(&lift); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE legal_holds SET version = version + 1 WHERE id = $1`, id); err != nil {
			return err
		}
		if err := holdAudit(r.Context(), tx, p, rid, "legal_hold.lift_requested", id, h.hospital,
			map[string]any{"lift_id": lift.String(), "reason": b.Reason}); err != nil {
			return err
		}
		out, err = readHoldDetail(r.Context(), tx, id, p.UserID)
		return err
	})
	if d.holdAnswer(w, r, p, "legal hold lift request", err) {
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// cancelMemberLifts cancels every open lift request of member (removed or
// demoted from platform admin in the same transaction), each audited
// legal_hold.lift_cancelled by the admin who changed the team.
func cancelMemberLifts(ctx context.Context, tx pgx.Tx, p authz.Principal, rid string, member uuid.UUID) error {
	rows, err := tx.Query(ctx, `UPDATE legal_hold_lifts SET cancelled_by = $2, cancelled_at = now()
		WHERE requested_by = $1 AND approved_at IS NULL AND cancelled_at IS NULL RETURNING id, hold_id`, member, p.UserID)
	if err != nil {
		return err
	}
	type ended struct{ lift, hold uuid.UUID }
	var all []ended
	for rows.Next() {
		var e ended
		if err := rows.Scan(&e.lift, &e.hold); err != nil {
			rows.Close()
			return err
		}
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range all {
		var hospital uuid.UUID
		if err := tx.QueryRow(ctx, `UPDATE legal_holds SET version = version + 1 WHERE id = $1 RETURNING hospital_id`,
			e.hold).Scan(&hospital); err != nil {
			return err
		}
		if err := holdAudit(ctx, tx, p, rid, "legal_hold.lift_cancelled", e.hold, hospital,
			map[string]any{"lift_id": e.lift.String()}); err != nil {
			return err
		}
	}
	return nil
}

type versionBody struct {
	Version    *int   `json:"version"`
	MFACode    string `json:"mfa_code"`
	BackupCode string `json:"backup_code"`
}

// openLift is the hold's open request (lapsed or not), for approve and
// cancel: errHoldConflict when there is none or the version is stale.
func openLift(ctx context.Context, tx pgx.Tx, id uuid.UUID, version int) (lockedHold, uuid.UUID, uuid.UUID, time.Time, error) {
	h, err := lockHold(ctx, tx, id)
	if err != nil {
		return h, uuid.Nil, uuid.Nil, time.Time{}, err
	}
	if h.lifted || h.version != version {
		return h, uuid.Nil, uuid.Nil, time.Time{}, errHoldConflict
	}
	var lift, by uuid.UUID
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT id, requested_by, expires_at FROM legal_hold_lifts WHERE hold_id = $1
		AND approved_at IS NULL AND cancelled_at IS NULL FOR UPDATE`, id).Scan(&lift, &by, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, uuid.Nil, uuid.Nil, time.Time{}, errHoldConflict
	}
	return h, lift, by, expires, err
}

// holdApprove is POST /platform/legal-holds/{id}/lift/approve: a different
// platform admin lifts the hold. The requester is refused before a code is
// spent (409 same_person).
func (d Deps) holdApprove(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathHold(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	var b versionBody
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &b) {
		return
	}
	if b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return
	}
	check := func(tx pgx.Tx) (lockedHold, uuid.UUID, error) {
		// The team lock, shared: a removal or demotion of the requester
		// (which cancels their requests) either committed before this
		// reads, or waits for it.
		if _, err := tx.Exec(r.Context(), `SELECT pg_advisory_xact_lock_shared(hashtext('mhai.platform_team'))`); err != nil {
			return lockedHold{}, uuid.Nil, err
		}
		h, lift, by, expires, err := openLift(r.Context(), tx, id, *b.Version)
		switch {
		case err != nil:
			return h, lift, err
		case by == p.UserID:
			return h, lift, errSamePerson
		case !expires.After(d.now()):
			return h, lift, errHoldExpired
		}
		// The requester must still be an active platform admin: a request
		// of someone deactivated, removed or demoted since is not one of the
		// two people.
		var still bool
		if err := tx.QueryRow(r.Context(), `SELECT EXISTS (SELECT 1 FROM users u WHERE u.id = $1 AND u.is_active
			AND u.hospital_id IS NULL AND EXISTS (SELECT 1 FROM user_roles x WHERE x.user_id = u.id AND x.role = 'platform_admin'))`,
			by).Scan(&still); err != nil {
			return h, lift, err
		}
		if !still {
			return h, lift, errHoldConflict
		}
		return h, lift, nil
	}
	if err := d.tx(r.Context(), func(tx pgx.Tx) error { _, _, err := check(tx); return err }); d.holdAnswer(w, r, p, "legal hold approve check", err) {
		return
	}
	if !d.holdRate(w, r, p) || !d.stepUp(w, r, b.MFACode, b.BackupCode) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var out LegalHoldDetail
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		h, lift, err := check(tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE legal_hold_lifts SET approved_by = $2, approved_at = now() WHERE id = $1`,
			lift, p.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE legal_holds SET lifted_at = now(), version = version + 1 WHERE id = $1`, id); err != nil {
			return err
		}
		if err := holdAudit(r.Context(), tx, p, rid, "legal_hold.lifted", id, h.hospital,
			map[string]any{"lift_id": lift.String()}); err != nil {
			return err
		}
		out, err = readHoldDetail(r.Context(), tx, id, p.UserID)
		return err
	})
	if d.holdAnswer(w, r, p, "legal hold approve", err) {
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// holdCancel is POST /platform/legal-holds/{id}/lift/cancel: either
// platform admin, no step-up. A lapsed request is ended the same way.
func (d Deps) holdCancel(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := pathHold(r)
	if !ok {
		httpx.Error(w, r, "not_found")
		return
	}
	var b struct {
		Version *int `json:"version"`
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &b) {
		return
	}
	if b.Version == nil || *b.Version < 1 {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.holdRate(w, r, p) {
		return
	}
	rid := httpx.RequestIDFrom(r.Context())
	var out LegalHoldDetail
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		h, lift, _, _, err := openLift(r.Context(), tx, id, *b.Version)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE legal_hold_lifts SET cancelled_by = $2, cancelled_at = now() WHERE id = $1`,
			lift, p.UserID); err != nil {
			return err
		}
		if _, err := tx.Exec(r.Context(), `UPDATE legal_holds SET version = version + 1 WHERE id = $1`, id); err != nil {
			return err
		}
		if err := holdAudit(r.Context(), tx, p, rid, "legal_hold.lift_cancelled", id, h.hospital,
			map[string]any{"lift_id": lift.String()}); err != nil {
			return err
		}
		out, err = readHoldDetail(r.Context(), tx, id, p.UserID)
		return err
	})
	if d.holdAnswer(w, r, p, "legal hold lift cancel", err) {
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}
