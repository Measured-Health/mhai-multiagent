// Package queries serves clinical questions (frontend contract §4, plan 3
// T12): POST /queries queues one as a pipeline.run_query job, GET
// /queries/{id} reads its status and GET /queries/{id}/result its answer.
//
// Every read and write goes through authz.Tx, so row-level security keeps
// each hospital to its own rows; a clinician sees only the questions they
// asked. Another hospital's or another clinician's id is 404 not_found,
// never 403. The question and the patient data are stored on the row only:
// they are never logged, audited or put on a NATS message (the job carries
// the query id).
package queries

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

// RunQuery is the job behind every question.
const RunQuery = "pipeline.run_query"

// Deps are what the routes need.
type Deps struct {
	Pool  *pgxpool.Pool
	Intel *intel.Client
	// Cap is the questions in flight per hospital (INTELLIGENCE_QUERY_CAP).
	Cap int
	Log *slog.Logger
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	if d.Intel != nil && d.Intel.Log != nil {
		return d.Intel.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Register adds the three routes to m. Only clinicians ask questions; the
// POST publishes pipeline.run_query (authz checks that the operation allows
// every role the route admits).
func Register(m *authz.Mux, d Deps) {
	clinician := []string{authz.RoleClinician}
	authz.Route(m, http.MethodPost, "/queries",
		authz.Rule{AnyRole: clinician, Write: true, Calls: []string{RunQuery}}, http.HandlerFunc(d.create))
	authz.Route(m, http.MethodGet, "/queries/{id}", authz.Rule{AnyRole: clinician}, http.HandlerFunc(d.status))
	authz.Route(m, http.MethodGet, "/queries/{id}/result", authz.Rule{AnyRole: clinician}, http.HandlerFunc(d.result))
}

// create queues a question: in one request transaction it takes an
// in-flight slot (429 rate_limited at the cap), inserts the queued row with
// a deadline of run_query's timeout, and audits query.submitted (ids and
// codes only). After the commit it publishes the job; a failed publish is
// logged and left to the publish sweeper. 202 {query_id, status: queued}.
func (d Deps) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, ok := authz.FromContext(ctx)
	if !ok {
		httpx.Error(w, r, "internal_error")
		return
	}
	var in QueryCreateIn
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !in.valid() {
		httpx.Error(w, r, "validation_failed")
		return
	}
	op, ok := authz.OperationByName(RunQuery)
	if !ok || d.Intel == nil || d.Pool == nil || d.Cap < 1 {
		httpx.Error(w, r, "not_ready")
		return
	}
	patient, err := json.Marshal(in.PatientData)
	if err != nil {
		httpx.Error(w, r, "validation_failed")
		return
	}
	id := uuid.New()
	rid := httpx.RequestIDFrom(ctx)
	cat := in.category()
	var deadline time.Time
	err = authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		// The account and hospital as they are now, not as the principal
		// cache saw them: FOR SHARE makes a concurrent deactivation wait
		// for this insert, so its trigger (00009) then cancels the row; a
		// deactivation that committed first is seen here and refused.
		var usable bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u JOIN hospitals h ON h.id = u.hospital_id
			WHERE u.id = $1 AND u.is_active AND h.status = 'active' FOR SHARE OF u)`, p.UserID).Scan(&usable); err != nil {
			return errInsert
		}
		if !usable {
			return errUnusable
		}
		if err := intel.ReserveInFlight(ctx, tx, p.HospitalID, d.Cap); err != nil {
			return err
		}
		var category any
		if cat != "" {
			category = cat
		}
		if err := tx.QueryRow(ctx, `INSERT INTO clinical_queries
			(id, hospital_id, patient_data_json, clinical_question, urgency, category, status, created_by, request_id, deadline_at)
			VALUES ($1, $2, $3::jsonb, $4, $5, $6, 'queued', $7, $8, now() + make_interval(secs => $9))
			RETURNING deadline_at`,
			id, p.HospitalID, string(patient), in.ClinicalQuestion, in.Urgency, category, p.UserID, rid, op.Timeout.Seconds()).
			Scan(&deadline); err != nil {
			return errInsert
		}
		payload := map[string]any{"query_id": id.String(), "urgency": in.Urgency}
		if cat != "" {
			payload["category"] = cat
		}
		return audit.Write(ctx, tx, audit.Event{ActorID: p.UserID.String(), HospitalID: p.HospitalID.String(), RequestID: rid,
			Action: "query.submitted", Entity: "query", EntityID: id.String(), Payload: payload})
	})
	if err != nil {
		code, _ := intel.Code(err)
		switch {
		case errors.Is(err, errUnusable):
			code = "unauthenticated"
		case !errors.Is(err, intel.ErrAtCap):
			code = "internal_error"
			d.log().Error("queries: submit failed", "request_id", rid, "hospital_id", p.HospitalID.String())
		}
		httpx.Error(w, r, code)
		return
	}
	d.publish(ctx, p, rid, id, deadline)
	httpx.JSON(w, http.StatusAccepted, QueryCreated{QueryID: id.String(), Status: "queued"})
}

var (
	errInsert   = errors.New("queries: insert failed")
	errUnusable = errors.New("queries: the account or hospital was deactivated")
)

// publish publishes the job after the commit, as the principal (their
// current roles), and marks the row published. Failures are logged by ids
// and left to the sweeper (spec §9.2), which republishes an unpublished
// row after 10 s; JetStream drops a duplicate within its window.
func (d Deps) publish(ctx context.Context, p authz.Principal, rid string, id uuid.UUID, deadline time.Time) {
	// The answer is already decided: finish even if the client went away.
	ctx = context.WithoutCancel(ctx)
	caller := intel.FromPrincipal(p, intel.JobRID(id, rid))
	if err := d.Intel.PublishJob(ctx, caller, RunQuery, id, &intelpb.RunQueryJob{QueryId: id.String()}, deadline); err != nil {
		d.log().Warn("queries: job not published; the sweeper will", "query_id", id.String(), "request_id", rid)
		return
	}
	if err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE clinical_queries SET published_at = now()
			WHERE id = $1 AND status = 'queued' AND published_at IS NULL`, id)
		return err
	}); err != nil {
		d.log().Warn("queries: published_at not set", "query_id", id.String(), "request_id", rid)
	}
}

// own reads the caller's own question id (path), or reports 404.
func own(w http.ResponseWriter, r *http.Request) (authz.Principal, uuid.UUID, bool) {
	p, ok := authz.FromContext(r.Context())
	if !ok {
		httpx.Error(w, r, "internal_error")
		return p, uuid.Nil, false
	}
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil || id.String() != raw {
		httpx.Error(w, r, "not_found")
		return p, uuid.Nil, false
	}
	return p, id, true
}

// status is GET /queries/{id}: the caller's own question, in its hospital
// (RLS), or 404.
func (d Deps) status(w http.ResponseWriter, r *http.Request) {
	p, id, ok := own(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var status string
	var progress float64
	var stage, errCode *string
	err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, progress, current_stage, error FROM clinical_queries
			WHERE id = $1 AND created_by = $2`, id, p.UserID).Scan(&status, &progress, &stage, &errCode)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, r, "not_found")
		return
	}
	if err != nil {
		httpx.Error(w, r, "internal_error")
		return
	}
	out, ok := statusOut(id.String(), status, progress, stage, errCode)
	if !ok {
		httpx.Error(w, r, "internal_error")
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// result is GET /queries/{id}/result: the answer of the caller's own
// completed question, or 404 (not theirs, not completed, no decision).
func (d Deps) result(w http.ResponseWriter, r *http.Request) {
	p, id, ok := own(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var (
		out                       ClinicalResultOut
		decision                  *string
		verified, degraded        *bool
		failed                    []string
		responseText, band        *string
		citations                 []byte
		retrievalMS, generationMS *float64
		flagPenalty               *float64
	)
	err := authz.Tx(ctx, d.Pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT d.hga_decision, d.verified, d.degraded, d.failed_agents, d.diagnosis, d.icd10_code,
			       d.response_text, d.trust_score::float8, d.trust_band, d.citations_json, d.retrieval_ms::float8, d.generation_ms::float8,
			       (SELECT e.flag_penalty::float8 FROM hga_evaluations e WHERE e.query_id = q.id ORDER BY e.id DESC LIMIT 1)
			FROM clinical_queries q JOIN clinical_decisions d ON d.query_id = q.id AND d.hospital_id = q.hospital_id
			WHERE q.id = $1 AND q.created_by = $2 AND q.status = 'completed'`, id, p.UserID).
			Scan(&decision, &verified, &degraded, &failed, &out.Diagnosis, &out.ICD10Code, &responseText, &out.TrustScore,
				&band, &citations, &retrievalMS, &generationMS, &flagPenalty)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.Error(w, r, "not_found")
		return
	}
	if err != nil || decision == nil || !decisions[*decision] {
		// No answer is invented: a stored decision outside the set is ours.
		httpx.Error(w, r, "internal_error")
		return
	}
	out.QueryID = id.String()
	out.Decision = *decision
	out.Verified = verified != nil && *verified
	// Unknown means degraded: never show an answer as fully checked when
	// the row doesn't say so.
	out.Degraded = degraded == nil || *degraded || !out.Verified
	out.FailedAgents = []string{}
	for _, a := range failed {
		if agents[a] {
			out.FailedAgents = append(out.FailedAgents, a)
		}
	}
	if responseText != nil {
		out.ResponseText = *responseText
	}
	out.ResponseAvailable = out.ResponseText != ""
	if band != nil && bands[*band] && out.Verified {
		out.TrustBand = band
	}
	// The pipeline stores the cited chunk ids only (no per-claim NLI or
	// excerpt yet), so citations stays empty rather than made up; the ids
	// are in cited_chunk_ids.
	out.Citations = []Citation{}
	out.CitedChunkIDs = []string{}
	var ids []any
	if len(citations) > 0 && json.Unmarshal(citations, &ids) == nil {
		for _, v := range ids {
			if s, ok := v.(string); ok && len(s) <= 128 {
				out.CitedChunkIDs = append(out.CitedChunkIDs, s)
			}
		}
	}
	if retrievalMS != nil {
		out.RetrievalMS = *retrievalMS
	}
	if generationMS != nil {
		out.GenerationMS = *generationMS
	}
	out.GuardrailDetails.L3NLI = L3NLI{ModelLoaded: out.Verified}
	if out.TrustScore != nil && out.Verified {
		fp := 0.0
		if flagPenalty != nil {
			fp = *flagPenalty
		}
		out.GuardrailDetails.L4Trust = &L4Trust{TrustScore: *out.TrustScore, BaseTrust: *out.TrustScore + fp, FlagPenalty: fp}
	}
	out.GuardrailDetails.CascadePass = out.Verified && out.Decision == "PASS"
	httpx.JSON(w, http.StatusOK, out)
}
