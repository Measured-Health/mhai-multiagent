package feedback

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
)

// The store is the only code that reads or writes feedback_events, and the
// one reader of the masked consultation (clinical_decisions.masked_question
// and masked_patient_json; C15). Every call runs in db.WithHospitalUser
// with the verified token's hospital and user: RLS keeps the hospital's
// rows, and 00011's triggers and policy check the user against the row.
// Nothing here reads a hospital or user from a request body.

var (
	// ErrUnusable: the account or its hospital was deactivated (the
	// principal cache may still hold it). 401.
	ErrUnusable = errors.New("feedback: the account or hospital is not active")
	// ErrNotFound: no such question or correction for this caller. 404.
	ErrNotFound = errors.New("feedback: not found")
	// ErrSelf: deciding your own correction. 403 self_approval.
	ErrSelf = errors.New("feedback: own correction")
	// ErrForbidden: withdrawing someone else's correction. 403.
	ErrForbidden = errors.New("feedback: not yours")
	// ErrDecided: the correction is no longer waiting. 409 already_decided.
	ErrDecided = errors.New("feedback: already decided")
	// ErrConflict: a live feedback exists, the decision has no masked copy,
	// or the database refused the decider (no longer an active clinician).
	// 409 conflict.
	ErrConflict = errors.New("feedback: conflict")
	// ErrInvalid: the body failed validation (after every guard). 422.
	ErrInvalid = errors.New("feedback: invalid body")
)

// Store reads and writes feedback as the caller.
type Store struct {
	Pool *pgxpool.Pool
}

func (s Store) tx(ctx context.Context, hid, me uuid.UUID, fn func(pgx.Tx) error) error {
	return db.WithHospitalUser(ctx, s.Pool, hid, me, fn)
}

// usable re-reads the caller's account and hospital: the principal cache
// may be up to 30 s old. FOR SHARE makes a concurrent deactivation wait.
func usable(ctx context.Context, tx pgx.Tx, me uuid.UUID) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users u JOIN hospitals h ON h.id = u.hospital_id
		WHERE u.id = $1 AND u.is_active AND h.status = 'active' FOR SHARE OF u)`, me).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrUnusable
	}
	return nil
}

// Submitted is what Submit recorded.
type Submitted struct {
	Status       string
	CorrectionID uuid.UUID // uuid.Nil for a validation
}

// Submit records the caller's feedback on their own completed question.
// Guards, in order: 404 (not the caller's, not completed, unknown), 409
// (a live feedback exists; a correction on a decision with no masked
// copy), then the body (in.validate, 422). The row and its audit fact
// (feedback.validated or correction.submitted, ids only) commit together.
func (s Store) Submit(ctx context.Context, hid, me uuid.UUID, rid string, queryID uuid.UUID, in FeedbackIn) (Submitted, error) {
	var out Submitted
	err := s.tx(ctx, hid, me, func(tx pgx.Tx) error {
		if err := usable(ctx, tx, me); err != nil {
			return err
		}
		// A reviewer can review only what they may read: the masked
		// question and a masked patient record with a numeric age, and a
		// decision in the contract's set.
		var reviewable bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(d.masked_question IS NOT NULL
			       AND jsonb_typeof(d.masked_patient_json -> 'age') = 'number'
			       AND d.hga_decision IN ('PASS', 'REGENERATE', 'ESCALATE'), false)
			FROM clinical_queries q
			LEFT JOIN clinical_decisions d ON d.query_id = q.id AND d.hospital_id = q.hospital_id
			WHERE q.id = $1 AND q.created_by = $2 AND q.status = 'completed'
			FOR SHARE OF q`, queryID, me).Scan(&reviewable)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM feedback_events WHERE query_id = $1 AND status <> 'withdrawn')`,
			queryID).Scan(&live); err != nil {
			return err
		}
		if live || (in.Action == "correct" && !reviewable) {
			return ErrConflict
		}
		sub, ok := in.validate(time.Now())
		if !ok {
			return ErrInvalid
		}
		id := uuid.New()
		status := "recorded"
		if sub.action == "correct" {
			status = "pending_review"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO feedback_events
			(id, hospital_id, query_id, submitted_by, action, status, corrected_diagnosis, reason, client_created_at, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			id, hid, queryID, me, sub.action, status, sub.diagnosis, sub.reason, sub.clientCreatedAt, nullable(rid)); err != nil {
			if pgCode(err) == "23505" { // feedback_one_live: a parallel submit won
				return ErrConflict
			}
			return err
		}
		e := audit.Event{ActorID: me.String(), HospitalID: hid.String(), RequestID: rid}
		if sub.action == "validate" {
			e.Action, e.Entity, e.EntityID = "feedback.validated", "query", queryID.String()
			e.Payload = map[string]any{"query_id": queryID.String()}
		} else {
			e.Action, e.Entity, e.EntityID = "correction.submitted", "correction", id.String()
			e.Payload = map[string]any{"correction_id": id.String(), "query_id": queryID.String()}
			out.CorrectionID = id
		}
		out.Status = status
		return audit.Write(ctx, tx, e)
	})
	if err != nil {
		return Submitted{}, err
	}
	return out, nil
}

// Decision is what the caller asks to do with a correction. Body is the
// raw request body, checked only after every guard.
type Decision struct {
	Kind string // approve, reject or withdraw
	Body []byte
}

// Decided is what Decide did. For an approval: JobID is the
// approve_correction job's id (the jobs row, and correction.approved's
// job_id), Deadline its deadline, and Publish whether it is within the
// hospital's in-flight budget (intel.QueueJob), so the caller publishes it
// after the commit; past the budget the sweeper does. Zero otherwise.
type Decided struct {
	JobID    uuid.UUID
	Deadline time.Time
	Publish  bool
}

// Decide approves, rejects or withdraws correction id as the caller.
// Guards, in order, under a row lock: 404 (unknown, not a correction,
// another hospital's), 403 (ErrSelf deciding your own; ErrForbidden
// withdrawing another's), 409 (ErrDecided: no longer waiting), then the
// body (ErrInvalid). A 403 or 409 commits its correction.decision_refused
// row and nothing else; a 404 or 422 writes nothing. The database refusing
// the decider (no longer an active clinician of the hospital) is
// ErrConflict.
//
// An approval is, in this one transaction: the conditional UPDATE, the
// queued approve_correction jobs row (approveJob, created by the caller,
// with the job id chosen here), and correction.approved naming that job.
// The caller publishes the job after the commit (spec §4.5).
func (s Store) Decide(ctx context.Context, hid, me, id uuid.UUID, rid string, d Decision) (Decided, error) {
	var out Decided
	var refused error
	err := s.tx(ctx, hid, me, func(tx pgx.Tx) error {
		if err := usable(ctx, tx, me); err != nil {
			return err
		}
		var submittedBy uuid.UUID
		var status string
		err := tx.QueryRow(ctx, `SELECT submitted_by, status FROM feedback_events WHERE id = $1 AND action = 'correct' FOR UPDATE`, id).
			Scan(&submittedBy, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var code string
		switch {
		case d.Kind != "withdraw" && submittedBy == me:
			refused, code = ErrSelf, "self_approval"
		case d.Kind == "withdraw" && submittedBy != me:
			refused, code = ErrForbidden, "forbidden"
		case status != "pending_review":
			refused, code = ErrDecided, "already_decided"
		}
		if refused != nil {
			// The refusal is audited and committed; nothing else changes.
			return audit.Write(ctx, tx, audit.Event{ActorID: me.String(), HospitalID: hid.String(), RequestID: rid,
				Action: "correction.decision_refused", Entity: "correction", EntityID: id.String(),
				Payload: map[string]any{"correction_id": id.String(), "reason": code, "attempted": d.Kind}})
		}

		e := audit.Event{ActorID: me.String(), HospitalID: hid.String(), RequestID: rid, Entity: "correction", EntityID: id.String()}
		var tag pgconn.CommandTag
		switch d.Kind {
		case "approve":
			if !emptyBody(d.Body) {
				return ErrInvalid
			}
			tag, err = tx.Exec(ctx, `UPDATE feedback_events SET status = 'approved', decided_by = $2, decided_at = now()
				WHERE id = $1 AND action = 'correct' AND status = 'pending_review' AND submitted_by <> $2`, id, me)
			out.JobID = uuid.New()
			e.Action = "correction.approved"
			e.Payload = map[string]any{"correction_id": id.String(), "job_id": out.JobID.String()}
		case "reject":
			reason, note, ok := parseReject(d.Body)
			if !ok {
				return ErrInvalid
			}
			tag, err = tx.Exec(ctx, `UPDATE feedback_events SET status = 'rejected', decided_by = $2, decided_at = now(),
				rejection_code = $3, rejection_note = $4
				WHERE id = $1 AND action = 'correct' AND status = 'pending_review' AND submitted_by <> $2`, id, me, reason, note)
			e.Action = "correction.rejected"
			e.Payload = map[string]any{"correction_id": id.String(), "reason_code": reason}
		case "withdraw":
			if !emptyBody(d.Body) {
				return ErrInvalid
			}
			tag, err = tx.Exec(ctx, `UPDATE feedback_events SET status = 'withdrawn', decided_at = now()
				WHERE id = $1 AND action = 'correct' AND status = 'pending_review' AND submitted_by = $2`, id, me)
			e.Action = "correction.withdrawn"
			e.Payload = map[string]any{"correction_id": id.String()}
		default:
			return errors.New("feedback: unknown decision")
		}
		if err != nil {
			if deciderRefused(err) {
				return ErrConflict
			}
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrDecided // unreachable under the row lock
		}
		if d.Kind == "approve" {
			if out.Deadline, out.Publish, err = approveJob(ctx, tx, hid, me, id, out.JobID, rid); err != nil {
				return err
			}
		}
		if err := audit.Write(ctx, tx, e); err != nil {
			return err
		}
		if d.Kind == "approve" && afterApproveAudit != nil {
			return afterApproveAudit()
		}
		return nil
	})
	if err != nil {
		return Decided{}, err
	}
	if refused != nil {
		return Decided{}, refused
	}
	return out, nil
}

// approveJob queues the approve_correction job for correction id in the
// approval's own transaction: created by the approver (the token's user),
// with the request id, the id correction.approved records, and a deadline
// of the operation's timeout (intel.QueueJob, which also says whether the
// job is within the hospital's budget).
func approveJob(ctx context.Context, tx pgx.Tx, hid, me, id, jobID uuid.UUID, rid string) (time.Time, bool, error) {
	return intel.QueueJob(ctx, tx, intel.NewJob{ID: jobID, HospitalID: hid, SubjectID: id, CreatedBy: me,
		Type: approveCorrectionType, RequestID: rid})
}

// MarkPublished sets the job's published_at once its publish succeeded,
// if it is still queued and unpublished.
func (s Store) MarkPublished(ctx context.Context, hid, me, jobID uuid.UUID) error {
	return s.tx(ctx, hid, me, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE jobs SET published_at = now() WHERE id = $1 AND status = 'queued' AND published_at IS NULL`, jobID)
		return err
	})
}

// deciderRefused reports whether err is 00011's decider check refusing the
// move (the decider is no longer an active clinician of the hospital):
// check_violation with the trigger's fixed message, which carries no ids.
func deciderRefused(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23514" &&
		strings.Contains(pe.Message, "the decider must be an active clinician of the hospital")
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// correctionSelect reads a correction with its people, the decision and
// the masked consultation. It never names the raw question or patient data
// columns (C15). where and order below are constants, never input.
const correctionSelect = `SELECT f.id, f.query_id, f.status, f.corrected_diagnosis, f.reason, f.created_at, f.decided_at,
	       f.rejection_code, f.rejection_note,
	       su.display_name, su.full_name, su.specialty,
	       du.display_name, du.full_name, du.specialty,
	       h.name,
	       d.hga_decision, d.verified, d.degraded, d.diagnosis, d.icd10_code, d.response_text, d.citations_json,
	       d.masked_question, d.masked_patient_json,
	       q.urgency, q.category
	FROM feedback_events f
	JOIN users su ON su.id = f.submitted_by
	LEFT JOIN users du ON du.id = f.decided_by
	JOIN hospitals h ON h.id = f.hospital_id
	JOIN clinical_queries q ON q.id = f.query_id AND q.hospital_id = f.hospital_id
	LEFT JOIN clinical_decisions d ON d.query_id = f.query_id AND d.hospital_id = f.hospital_id
	WHERE f.action = 'correct' AND `

const (
	pendingWhere  = `f.status = 'pending_review' AND f.submitted_by <> $1 ORDER BY f.created_at, f.id LIMIT 100`
	mineWhere     = `f.submitted_by = $1 ORDER BY f.created_at DESC, f.id LIMIT 200`
	reviewedWhere = `f.decided_by = $1 AND f.decided_at > now() - interval '30 days' ORDER BY f.decided_at DESC, f.id LIMIT 50`
)

// ListPending is the hospital's waiting corrections, never the caller's
// own, oldest first, at most 100.
func (s Store) ListPending(ctx context.Context, hid, me uuid.UUID) ([]CorrectionOut, error) {
	return s.list(ctx, hid, me, pendingWhere)
}

// ListMine is the caller's own corrections, every status, newest first, at
// most 200.
func (s Store) ListMine(ctx context.Context, hid, me uuid.UUID) ([]CorrectionOut, error) {
	return s.list(ctx, hid, me, mineWhere)
}

// ListReviewed is the corrections the caller decided in the last 30 days,
// newest first, at most 50.
func (s Store) ListReviewed(ctx context.Context, hid, me uuid.UUID) ([]CorrectionOut, error) {
	return s.list(ctx, hid, me, reviewedWhere)
}

func (s Store) list(ctx context.Context, hid, me uuid.UUID, where string) ([]CorrectionOut, error) {
	out := []CorrectionOut{}
	err := s.tx(ctx, hid, me, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, correctionSelect+where, me)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id, queryID                    uuid.UUID
				status                         string
				diagnosis, reason              *string
				created                        time.Time
				decidedAt                      *time.Time
				rejCode, rejNote               *string
				subName                        string
				subFull, subSpec               *string
				decName, decFull, decSpec      *string
				hospital                       string
				decision                       *string
				verified, degraded             *bool
				dDiagnosis, dICD, responseText *string
				cites                          []byte
				maskedQuestion                 *string
				maskedPatient                  []byte
				urgency                        string
				category                       *string
			)
			if err := rows.Scan(&id, &queryID, &status, &diagnosis, &reason, &created, &decidedAt, &rejCode, &rejNote,
				&subName, &subFull, &subSpec, &decName, &decFull, &decSpec, &hospital,
				&decision, &verified, &degraded, &dDiagnosis, &dICD, &responseText, &cites,
				&maskedQuestion, &maskedPatient, &urgency, &category); err != nil {
				return err
			}
			c := CorrectionOut{
				ID: id.String(), QueryID: queryID.String(), Status: status,
				SubmittedBy: PersonOut{DisplayName: subName, FullName: subFull, Specialty: subSpec, Hospital: &hospital},
				SubmittedAt: isoTime(created),
				Original: OriginalOut{Decision: "ESCALATE", Degraded: true, Diagnosis: dDiagnosis, ICD10Code: dICD,
					Citations: citations(cites)},
				Consultation: consultation(maskedQuestion, maskedPatient, urgency, category),
			}
			if diagnosis != nil {
				c.CorrectedDiagnosis = *diagnosis
			}
			if reason != nil {
				c.Reason = *reason
			}
			// No answer is shown as checked unless the row says so; an
			// unknown decision escalates.
			if decision != nil && decisions[*decision] {
				c.Original.Decision = *decision
				c.Original.Degraded = degraded == nil || *degraded || verified == nil || !*verified
			}
			if responseText != nil {
				c.Original.ResponseText = *responseText
			}
			if decName != nil {
				c.DecidedBy = &PersonOut{DisplayName: *decName, FullName: decFull, Specialty: decSpec, Hospital: &hospital}
			}
			if decidedAt != nil {
				t := isoTime(*decidedAt)
				c.DecidedAt = &t
			}
			if status == "rejected" && rejCode != nil {
				c.Rejection = &RejectionOut{ReasonCode: *rejCode, Note: rejNote}
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Stats is the caller's own feedback counts.
func (s Store) Stats(ctx context.Context, hid, me uuid.UUID) (FeedbackStatsOut, error) {
	var st FeedbackStatsOut
	err := s.tx(ctx, hid, me, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE action = 'validate'),
			count(*) FILTER (WHERE action = 'correct' AND status = 'pending_review'),
			count(*) FILTER (WHERE action = 'correct' AND status = 'approved'),
			count(*) FILTER (WHERE action = 'correct' AND status = 'rejected'),
			count(*) FILTER (WHERE action = 'correct' AND status = 'withdrawn')
			FROM feedback_events WHERE submitted_by = $1`, me).
			Scan(&st.Agreed, &st.CorrectionsWaiting, &st.CorrectionsApproved, &st.CorrectionsRejected, &st.CorrectionsWithdrawn)
	})
	return st, err
}

// afterApproveAudit is a test hook (export_test.go): nil in production.
var afterApproveAudit func() error
