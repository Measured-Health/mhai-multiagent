package platform

// The chain check and the auditors' writes (Plan 4 spec §5.2, §5.4;
// contract §14.9 "Integrity"):
//
//   - POST /platform/audit/verify: start a check, or get the running one.
//   - GET /platform/audit/verify/runs, /verify/runs/{id}, /verify/status:
//     reads, each recorded (audit.searched {searched_kind: run}).
//   - POST /platform/audit/verify/runs/{id}/outcome: an auditor's note on a
//     broken run.
//   - POST /platform/audit/undisplayable: the app's report of entries it
//     couldn't show.
//
// The writes are AuditorWrite routes, the only unsafe routes an auditor
// may call (TestAuditorWriteRoutesPinned).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// SecurityContact is who the integrity page names (configuration:
// SECURITY_CONTACT_NAME, SECURITY_CONTACT_EMAIL).
type SecurityContact struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// OK reports whether c is set and of a shape mhai-web reads.
func (c SecurityContact) OK() bool {
	if c.Name == "" || c.Email == "" || len(c.Email) > 254 || strings.TrimSpace(c.Name) != c.Name {
		return false
	}
	a, err := mail.ParseAddress(c.Email)
	return err == nil && a.Address == c.Email && a.Name == "" && !pseudonymIn.MatchString(c.Name)
}

func registerVerify(m *authz.Mux, d Deps) {
	both := []string{authz.RolePlatformAdmin, authz.RoleAuditor}
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: both}
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: both, AuditorWrite: true}
	auditorWrite := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RoleAuditor}, AuditorWrite: true}
	authz.Route(m, http.MethodPost, "/platform/audit/verify", write, http.HandlerFunc(d.startVerify))
	authz.Route(m, http.MethodPost, "/platform/audit/undisplayable", write, http.HandlerFunc(d.undisplayable))
	authz.Route(m, http.MethodPost, "/platform/audit/verify/runs/{id}/outcome", auditorWrite, http.HandlerFunc(d.outcome))
	authz.Route(m, http.MethodGet, "/platform/audit/verify/runs", read, http.HandlerFunc(d.runs))
	authz.Route(m, http.MethodGet, "/platform/audit/verify/runs/{id}", read, http.HandlerFunc(d.run))
	authz.Route(m, http.MethodGet, "/platform/audit/verify/status", read, http.HandlerFunc(d.status))
}

// walk runs a started run's walk in the background: d.Walk, or the walk
// with the 10-minute budget on a context that outlives the request.
func (d Deps) walk(id uuid.UUID) {
	if d.Walk != nil {
		d.Walk(id)
		return
	}
	go func() {
		if _, err := audit.Execute(context.Background(), d.Pool, id, audit.VerifyBudget); err != nil {
			d.log().Error("platform: audit check failed", "run_id", id.String(), "reason", "internal_error")
		}
	}()
}

// emptyBody accepts no body, or the empty object, and nothing else.
func emptyBody(r *http.Request) bool {
	if r.Body == nil {
		return true
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1024))
	if err != nil || len(b) == 1024 {
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var v struct{}
	if dec.Decode(&v) != nil {
		return false
	}
	_, err = dec.Token()
	return errors.Is(err, io.EOF)
}

// startVerify is POST /platform/audit/verify → {run_id}, the new run or the
// running one.
func (d Deps) startVerify(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" || !emptyBody(r) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var id uuid.UUID
	var started bool
	by := p.UserID
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		id, started, err = audit.StartRunTx(r.Context(), tx, &by, httpx.RequestIDFrom(r.Context()))
		return err
	})
	if err != nil {
		d.fail(w, r, p, "verify start")
		return
	}
	if started {
		d.walk(id)
	}
	httpx.JSON(w, http.StatusOK, struct {
		RunID string `json:"run_id"`
	}{id.String()})
}

// runs is GET /platform/audit/verify/runs?cursor → {runs, next_cursor}.
func (d Deps) runs(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	v, err := params(r, []string{"cursor"})
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var after *audit.RunCursor
	if c, ok := v["cursor"]; ok {
		cur, err := audit.DecodeRunCursor(c)
		if err != nil {
			httpx.Error(w, r, "validation_failed")
			return
		}
		after = &cur
	}
	var out []audit.RunOut
	var next string
	err = d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, next, err = audit.ListRuns(r.Context(), tx, after); err != nil {
			return err
		}
		_, err = audit.RecordRunsRead(r.Context(), tx, reader(r, p), "runs", v["cursor"])
		return err
	})
	if err != nil {
		d.fail(w, r, p, "runs")
		return
	}
	var nc *string
	if next != "" {
		nc = &next
	}
	httpx.JSON(w, http.StatusOK, struct {
		Runs       []audit.RunOut `json:"runs"`
		NextCursor *string        `json:"next_cursor"`
	}{out, nc})
}

// runID is the path's run id: a canonical uuid, or 404.
func runID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	s := r.PathValue("id")
	if !canonicalUUID(s) {
		httpx.Error(w, r, "not_found")
		return uuid.Nil, false
	}
	return uuid.MustParse(s), true
}

// run is GET /platform/audit/verify/runs/{id}.
func (d Deps) run(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := runID(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out audit.RunOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if out, err = audit.GetRun(r.Context(), tx, id); err != nil {
			return err
		}
		_, err = audit.RecordRunsRead(r.Context(), tx, reader(r, p), "run", id.String())
		return err
	})
	switch {
	case errors.Is(err, audit.ErrNotFound):
		httpx.Error(w, r, "not_found")
	case err != nil:
		d.fail(w, r, p, "run")
	default:
		httpx.JSON(w, http.StatusOK, out)
	}
}

// status is GET /platform/audit/verify/status → {latest, running,
// open_failure, security_contact, oldest_kept_at}. Without a configured
// contact it fails closed: 503 not_ready.
func (d Deps) status(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !d.SecurityContact.OK() {
		d.log().Error("platform: audit status refused: no security contact", "request_id", httpx.RequestIDFrom(r.Context()),
			"reason", "not_ready")
		httpx.Error(w, r, "not_ready")
		return
	}
	var st audit.IntegrityStatus
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		if st, err = audit.Status(r.Context(), tx); err != nil {
			return err
		}
		_, err = audit.RecordRunsRead(r.Context(), tx, reader(r, p), "status", "")
		return err
	})
	if err != nil {
		d.fail(w, r, p, "status")
		return
	}
	httpx.JSON(w, http.StatusOK, struct {
		Latest          *audit.RunOut   `json:"latest"`
		Running         *audit.RunOut   `json:"running"`
		OpenFailure     *audit.RunOut   `json:"open_failure"`
		SecurityContact SecurityContact `json:"security_contact"`
		OldestKeptAt    string          `json:"oldest_kept_at"`
	}{st.Latest, st.Running, st.OpenFailure, d.SecurityContact, st.OldestKeptAt})
}

// outcome is POST /platform/audit/verify/runs/{id}/outcome {note, version}
// → the run. Auditors only (the route's roles); broken runs only (409
// conflict otherwise, and on a stale version or a second note). The note
// is stored on the run row; the chain gets audit.check_outcome {run_id}
// only, and no log line carries it.
func (d Deps) outcome(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := runID(w, r)
	if !ok {
		return
	}
	var body struct {
		Note    *string `json:"note"`
		Version *int    `json:"version"`
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	if body.Note == nil || body.Version == nil || *body.Version < 1 || !audit.OutcomeNoteOK(*body.Note) {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var out audit.RunOut
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		out, err = audit.AddOutcome(r.Context(), tx, reader(r, p), id, *body.Note, *body.Version)
		return err
	})
	switch {
	case errors.Is(err, audit.ErrNotFound):
		httpx.Error(w, r, "not_found")
		return
	case errors.Is(err, audit.ErrConflict):
		httpx.Error(w, r, "conflict")
		return
	case err != nil:
		d.fail(w, r, p, "outcome")
		return
	}
	// The note may close the open failure: clear the alert if so.
	if err := audit.RefreshBrokenGauge(r.Context(), d.Pool); err != nil {
		d.log().Error("platform: audit gauge not refreshed", "request_id", httpx.RequestIDFrom(r.Context()), "reason", "internal_error")
	}
	httpx.JSON(w, http.StatusOK, out)
}

// undisplayable is POST /platform/audit/undisplayable {ids}: 1 to 50 event
// ids → 204, one audit.undisplayable_reported.
func (d Deps) undisplayable(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	if !httpx.Decode(w, r, &body) {
		return
	}
	if len(body.IDs) < 1 || len(body.IDs) > audit.UndisplayableMax {
		httpx.Error(w, r, "validation_failed")
		return
	}
	seqs := make([]int64, 0, len(body.IDs))
	for _, s := range body.IDs {
		seq, ok := audit.EventID(s)
		if !ok {
			httpx.Error(w, r, "validation_failed")
			return
		}
		seqs = append(seqs, seq)
	}
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		return audit.RecordUndisplayable(r.Context(), tx, reader(r, p), seqs)
	})
	if err != nil {
		d.fail(w, r, p, "undisplayable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
