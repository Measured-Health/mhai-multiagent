package alerts

// Possible duplicates and exclusions (design §10.2, §10.6, §16.2; plan Task
// 34): hospital admins only, their own hospital only (another hospital's
// match or patient is 404, A31). Nothing merges by itself: a merge is a
// hospital admin's confirm, with a fresh step-up code, through core's
// definer emr_merge_patients(), audited in the same transaction (A12). An
// exclusion withdraws the patient's open alerts at once and purges the
// patient's data in the same transaction (A34). A merge or an exclusion
// chosen as a deadlock victim against the emr service is retried, then
// answered 503 not_ready; nothing is left half-done.

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

// maxMatches bounds one list.
const maxMatches = 100

// PatientCard is one side of a possible duplicate.
type PatientCard struct {
	ID       string   `json:"id"`
	FullName string   `json:"full_name"`
	MRN      *string  `json:"mrn"`
	Age      *int     `json:"age"`
	Sex      string   `json:"sex"`
	Sources  []string `json:"sources"`
	Records  int64    `json:"records"`
}

// MatchOut is one possible duplicate.
type MatchOut struct {
	ID         string      `json:"id"`
	Reason     string      `json:"reason"`
	A          PatientCard `json:"a"`
	B          PatientCard `json:"b"`
	ProposedAt string      `json:"proposed_at"`
}

// MatchesOut is the list.
type MatchesOut struct {
	Matches []MatchOut `json:"matches"`
}

// matchesList is GET /hospital/patient-matches: undecided matches of two
// active patients, oldest first. The cards carry names and MRNs, so a page
// served is audited (emr.patient_matches_read, the ids).
func (d Deps) matchesList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	now := d.now()
	out := MatchesOut{Matches: []MatchOut{}}
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		rows, err := tx.Query(ctx, `SELECT m.id, m.reason, m.proposed_at, m.patient_a, m.patient_b
			FROM emr.possible_matches m
			JOIN emr.patients a ON a.hospital_id = m.hospital_id AND a.id = m.patient_a AND a.status = 'active'
			JOIN emr.patients b ON b.hospital_id = m.hospital_id AND b.id = m.patient_b AND b.status = 'active'
			WHERE m.hospital_id = $1 AND m.decision IS NULL ORDER BY m.proposed_at, m.id LIMIT $2`, p.HospitalID, maxMatches)
		if err != nil {
			return err
		}
		type pair struct {
			out  MatchOut
			a, b uuid.UUID
		}
		var pairs []pair
		var pids []uuid.UUID
		for rows.Next() {
			var x pair
			var id uuid.UUID
			var at time.Time
			if err := rows.Scan(&id, &x.out.Reason, &at, &x.a, &x.b); err != nil {
				rows.Close()
				return err
			}
			x.out.ID, x.out.ProposedAt = id.String(), stamp(at)
			pairs = append(pairs, x)
			pids = append(pids, x.a, x.b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(pairs) == 0 {
			return nil
		}
		cards, err := patientCards(ctx, tx, p.HospitalID, pids, now)
		if err != nil {
			return err
		}
		served := make([]string, 0, len(pairs))
		for _, x := range pairs {
			ca, okA := cards[x.a]
			cb, okB := cards[x.b]
			if !okA || !okB {
				continue
			}
			x.out.A, x.out.B = ca, cb
			out.Matches = append(out.Matches, x.out)
			served = append(served, x.out.ID)
		}
		if len(served) == 0 {
			return nil
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "emr.patient_matches_read", Entity: "user",
			EntityID: p.UserID.String(), Payload: map[string]any{"match_ids": served}})
	})
	if err != nil {
		d.fail(w, r, p, "matches read", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// patientCards reads the cards of pids (the caller's hospital only).
func patientCards(ctx context.Context, tx pgx.Tx, hid uuid.UUID, pids []uuid.UUID, now time.Time) (map[uuid.UUID]PatientCard, error) {
	out := map[uuid.UUID]PatientCard{}
	rows, err := tx.Query(ctx, `SELECT p.id, p.display_name, p.mrn, p.birth_date, p.sex, s.sources, s.records
		FROM emr.patients p JOIN emr_patient_sources($2::uuid[]) s ON s.patient_id = p.id
		WHERE p.hospital_id = $1 AND p.id = ANY ($2::uuid[])`, hid, pids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var c PatientCard
		var name *string
		var birth *time.Time
		if err := rows.Scan(&id, &name, &c.MRN, &birth, &c.Sex, &c.Sources, &c.Records); err != nil {
			return nil, err
		}
		c.ID = id.String()
		if name != nil {
			c.FullName = *name
		}
		if birth != nil {
			a := ageAt(*birth, now)
			c.Age = &a
		}
		if c.Sources == nil {
			c.Sources = []string{}
		}
		out[id] = c
	}
	return out, rows.Err()
}

// stepUpIn is the step-up code every match and exclusion write carries.
type stepUpIn struct {
	MFACode    string `json:"mfa_code"`
	BackupCode string `json:"backup_code"`
}

// stepUp checks a fresh code (a refusal is audited by auth); false means
// the reply was sent.
func (d Deps) stepUp(w http.ResponseWriter, r *http.Request, in stepUpIn) bool {
	if d.stepUpCheck != nil {
		if in.MFACode == "" && in.BackupCode == "" {
			httpx.Error(w, r, "mfa_required")
			return false
		}
		if d.stepUpCheck(r, in.MFACode, in.BackupCode) != nil {
			httpx.Error(w, r, "mfa_invalid")
			return false
		}
		return true
	}
	if d.Auth == nil {
		httpx.Error(w, r, "not_ready")
		return false
	}
	if in.MFACode == "" && in.BackupCode == "" {
		httpx.Error(w, r, "mfa_required")
		return false
	}
	if err := d.Auth.StepUpAudited(r, in.MFACode, in.BackupCode); err != nil {
		d.Auth.WriteStepUpError(w, r, err)
		return false
	}
	return true
}

// undecided reports whether match id is an undecided match of the caller's
// hospital (else the route answers 404 before asking for the code).
func (d Deps) undecided(ctx context.Context, hid, id uuid.UUID) (bool, error) {
	var n int
	err := d.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM emr.possible_matches WHERE hospital_id = $1 AND id = $2
			AND decision IS NULL`, hid, id).Scan(&n)
	})
	return n == 1, err
}

type confirmIn struct {
	Keep *string `json:"keep"`
	stepUpIn
}

// matchConfirm is POST /hospital/patient-matches/{id}/confirm {keep: a|b}.
// Order: the path (404), the body (422), the match (404), the code (403);
// then the merge and its audit row, in one transaction.
func (d Deps) matchConfirm(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var in confirmIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Keep == nil || (*in.Keep != "a" && *in.Keep != "b") {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if found, err := d.undecided(r.Context(), p.HospitalID, id); err != nil || !found {
		if err == nil {
			err = errNotFound
		}
		d.fail(w, r, p, "match confirm", err)
		return
	}
	if !d.stepUp(w, r, in.stepUpIn) {
		return
	}
	err := d.retryTx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		var a, b uuid.UUID
		err := tx.QueryRow(ctx, `SELECT patient_a, patient_b FROM emr.possible_matches WHERE hospital_id = $1 AND id = $2`,
			p.HospitalID, id).Scan(&a, &b)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		kept, merged := a, b
		if *in.Keep == "b" {
			kept, merged = b, a
		}
		var mergeID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT emr_merge_patients($1, $2)`, id, *in.Keep).Scan(&mergeID); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "emr.patients_merged", Entity: "patient_match", EntityID: id.String(),
			Payload: map[string]any{"match_id": id.String(), "merge_id": mergeID.String(),
				"kept_patient_id": kept.String(), "merged_patient_id": merged.String()}})
	})
	if err != nil {
		d.fail(w, r, p, "match confirm", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// matchReject is POST /hospital/patient-matches/{id}/reject: the pair is
// not proposed again for the same reason.
func (d Deps) matchReject(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	var in stepUpIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if found, err := d.undecided(r.Context(), p.HospitalID, id); err != nil || !found {
		if err == nil {
			err = errNotFound
		}
		d.fail(w, r, p, "match reject", err)
		return
	}
	if !d.stepUp(w, r, in) {
		return
	}
	err := d.retryTx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		if _, err := tx.Exec(ctx, `SELECT emr_reject_match($1)`, id); err != nil {
			return err
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Action: "emr.patient_match_rejected", Entity: "patient_match",
			EntityID: id.String(), Payload: map[string]any{"match_id": id.String()}})
	})
	if err != nil {
		d.fail(w, r, p, "match reject", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ExclusionReasons are the closed exclusion reasons.
var ExclusionReasons = []string{"objection", "hospital_policy", "other"}

type excludeIn struct {
	MRN       *string `json:"mrn"`
	PatientID *string `json:"patient_id"`
	Reason    *string `json:"reason"`
	stepUpIn
}

// ExclusionOut is the 202 reply: the patient, and when its data is gone.
// Purged in the same transaction: purge_by is the request's time and
// purge_held false. Under a legal hold nothing is deleted: purge_held is
// true and purge_by the request's due time, which the hold suspends.
type ExclusionOut struct {
	PatientID string `json:"patient_id"`
	PurgeBy   string `json:"purge_by"`
	PurgeHeld bool   `json:"purge_held"`
}

// errAmbiguous: an MRN names more than one active patient.
var errAmbiguous = errors.New("alerts: ambiguous mrn")

// resolvePatient finds the one active patient named by the body.
func (d Deps) resolvePatient(ctx context.Context, hid uuid.UUID, in excludeIn) (uuid.UUID, error) {
	var found []uuid.UUID
	err := d.tx(ctx, func(tx pgx.Tx) error {
		var rows pgx.Rows
		var err error
		if in.PatientID != nil {
			pid, ok := canonicalID(*in.PatientID)
			if !ok {
				return errNotFound
			}
			rows, err = tx.Query(ctx, `SELECT id FROM emr.patients WHERE hospital_id = $1 AND id = $2 AND status = 'active'`, hid, pid)
		} else {
			rows, err = tx.Query(ctx, `SELECT id FROM emr.patients WHERE hospital_id = $1 AND mrn = $2 AND status = 'active'
				ORDER BY id LIMIT 2`, hid, *in.MRN)
		}
		if err != nil {
			return err
		}
		found, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	switch {
	case err != nil:
		return uuid.Nil, err
	case len(found) == 0:
		return uuid.Nil, errNotFound
	case len(found) > 1:
		return uuid.Nil, errAmbiguous
	}
	return found[0], nil
}

// exclude is POST /hospital/patient-exclusions {mrn} | {patient_id}, reason
// (design §10.6): 202. Order: the body (422), the patient (404; 409
// conflict when the MRN names two patients: send the patient id), the code
// (403); then the exclusion, its audit row and one alert.withdrawn per
// open alert, in one transaction.
func (d Deps) exclude(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	var in excludeIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if r.URL.RawQuery != "" || (in.MRN == nil) == (in.PatientID == nil) || in.Reason == nil ||
		!slices.Contains(ExclusionReasons, *in.Reason) || (in.MRN != nil && (len(*in.MRN) == 0 || len(*in.MRN) > 64)) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	pid, err := d.resolvePatient(r.Context(), p.HospitalID, in)
	switch {
	case errors.Is(err, errAmbiguous):
		httpx.Error(w, r, "conflict")
		return
	case err != nil:
		d.fail(w, r, p, "exclusion", err)
		return
	}
	if !d.stepUp(w, r, in.stepUpIn) {
		return
	}
	var x ExclusionResult
	err = d.retryTx(r.Context(), func(tx pgx.Tx) error {
		ctx := r.Context()
		var err error
		if x, err = ExcludePatient(ctx, tx, AuditBy{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(),
			RequestID: httpx.RequestIDFrom(ctx), Via: "route"}, pid, *in.Reason); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		d.fail(w, r, p, "exclusion", err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, ExclusionOut{PatientID: pid.String(), PurgeBy: stamp(x.PurgeBy), PurgeHeld: x.Held})
}

// ExclusionResult is what an exclusion did: the purge request, whether it
// is held, when the data is (or is due to be) gone, the counts and the
// alerts withdrawn.
type ExclusionResult struct {
	RequestID uuid.UUID
	Held      bool
	PurgeBy   time.Time
	Withdrawn []uuid.UUID
	Counts    map[string]int64
}

// AuditBy is who the exclusion's audit rows name: the actor (none from the
// operator CLI), the hospital, the request id and the path ("route" or
// "cli").
type AuditBy struct {
	ActorID, HospitalID, RequestID, Via string
}

// ExcludePatient excludes patient pid in tx (the hospital set; as the
// gateway or the operator's mhai_emr_admin) through emr_exclude_patient,
// and writes its audit rows in the same transaction: emr.patient_excluded
// (counts), one alert.withdrawn per alert withdrawn, and
// emr.exclusion_purged (counts) or, under a legal hold,
// emr.exclusion_purge_held. Ids, codes and counts only.
func ExcludePatient(ctx context.Context, tx pgx.Tx, by AuditBy, pid uuid.UUID, reason string) (ExclusionResult, error) {
	var x ExclusionResult
	var o, m, e, raw, matches, findings, cleared int64
	if err := tx.QueryRow(ctx, `SELECT observations, medications, encounters, raw_records, matches, alert_ids, findings,
		request_id, held, purge_by, alerts_cleared FROM emr_exclude_patient($1, $2)`, pid, reason).
		Scan(&o, &m, &e, &raw, &matches, &x.Withdrawn, &findings, &x.RequestID, &x.Held, &x.PurgeBy, &cleared); err != nil {
		return x, err
	}
	x.Counts = map[string]int64{"observations": o, "medications": m, "encounters": e, "raw_records": raw, "matches": matches,
		"findings": findings, "alerts": int64(len(x.Withdrawn))}
	event := func(action, entity, id string, payload map[string]any) audit.Event {
		return audit.Event{ActorID: by.ActorID, HospitalID: by.HospitalID, RequestID: by.RequestID, Action: action,
			Entity: entity, EntityID: id, Payload: payload}
	}
	excluded := map[string]any{"patient_id": pid.String(), "reason": reason, "via": by.Via, "request_id": x.RequestID.String(),
		"held": x.Held}
	for k, n := range x.Counts {
		excluded[k] = n
	}
	if err := audit.Write(ctx, tx, event("emr.patient_excluded", "patient", pid.String(), excluded)); err != nil {
		return x, err
	}
	for _, a := range x.Withdrawn {
		if err := audit.Write(ctx, tx, event("alert.withdrawn", "alert", a.String(),
			map[string]any{"alert_id": a.String(), "reason": "excluded"})); err != nil {
			return x, err
		}
	}
	purge := map[string]any{"request_id": x.RequestID.String(), "patient_id": pid.String(), "via": by.Via}
	if x.Held {
		return x, audit.Write(ctx, tx, event("emr.exclusion_purge_held", "purge_request", x.RequestID.String(), purge))
	}
	for _, k := range []string{"observations", "medications", "encounters", "raw_records", "matches", "findings"} {
		purge[k] = x.Counts[k]
	}
	// The alerts the purge took the patient from (withdrawn or handled).
	purge["alerts"] = cleared
	return x, audit.Write(ctx, tx, event("emr.exclusion_purged", "purge_request", x.RequestID.String(), purge))
}
