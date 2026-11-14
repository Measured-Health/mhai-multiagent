package hospital

// GET /hospital/review-oversight and GET
// /hospital/corrections/{id}/eligible-reviewers (frontend requirements
// §5.4). No clinical content: a waiting correction is its id, a short
// reference, who suggested it, its consultation's specialty and when; never
// its reason, its suggested diagnosis or the question.
//
// Core has no reviewer assignment or reminders yet: every active clinician
// but the submitter may review any waiting correction, assigned_to is
// always null (00011's assignee column is never written) and reminded_at
// is null. POST .../remind and .../assign answer 501 not_implemented.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// maxOversightItems bounds the waiting list (oldest first).
const maxOversightItems = 200

// OversightItem is OversightItemOut.
type OversightItem struct {
	ID                string     `json:"id"`
	Ref               string     `json:"ref"`
	SuggestedBy       PersonOut  `json:"suggested_by"`
	Specialty         *string    `json:"specialty"`
	SubmittedAt       string     `json:"submitted_at"`
	EligibleReviewers int        `json:"eligible_reviewers"`
	AssignedTo        *PersonOut `json:"assigned_to"`
	RemindedAt        *string    `json:"reminded_at"`
}

// SpecialtyReviewers is one specialty's active clinicians.
type SpecialtyReviewers struct {
	Specialty string `json:"specialty"`
	Reviewers int    `json:"reviewers"`
}

// OversightOut is the contract's OversightOut.
type OversightOut struct {
	OverdueAfterHours    int                  `json:"overdue_after_hours"`
	Waiting              int                  `json:"waiting"`
	MedianDecisionHours  *float64             `json:"median_decision_hours"`
	DecidedThisWeek      int                  `json:"decided_this_week"`
	Items                []OversightItem      `json:"items"`
	ReviewersBySpecialty []SpecialtyReviewers `json:"reviewers_by_specialty"`
}

// Ref is a correction's short reference: C- and its id's first six hex
// digits.
func Ref(id uuid.UUID) string { return "C-" + strings.ToUpper(id.String()[:6]) }

func (d Deps) oversight(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	ctx := r.Context()
	out := OversightOut{Items: []OversightItem{}, ReviewersBySpecialty: []SpecialtyReviewers{}}
	now := d.now()
	err := d.tx(ctx, func(tx pgx.Tx) error {
		s, err := loadSettings(ctx, tx, p.HospitalID, false)
		if err != nil {
			return err
		}
		out.OverdueAfterHours = s.ReviewOverdueHours
		if err := tx.QueryRow(ctx, `SELECT
				count(*) FILTER (WHERE status = 'pending_review'),
				count(*) FILTER (WHERE decided_at >= $2),
				percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM decided_at - created_at) / 3600.0)
					FILTER (WHERE decided_at >= $3)
			FROM feedback_events WHERE hospital_id = $1 AND action = 'correct'`,
			p.HospitalID, now.Add(-7*24*time.Hour), now.Add(-30*24*time.Hour)).
			Scan(&out.Waiting, &out.DecidedThisWeek, &out.MedianDecisionHours); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT f.id, f.created_at, s.display_name, q.category, a.display_name,
				(SELECT count(*) FROM users c WHERE c.hospital_id = f.hospital_id AND c.is_active AND c.id <> f.submitted_by
				   AND EXISTS (SELECT 1 FROM user_roles cr WHERE cr.user_id = c.id AND cr.role = 'clinician'))
			FROM feedback_events f
			JOIN users s ON s.id = f.submitted_by
			JOIN clinical_queries q ON q.id = f.query_id AND q.hospital_id = f.hospital_id
			LEFT JOIN users a ON a.id = f.assigned_to
			WHERE f.hospital_id = $1 AND f.action = 'correct' AND f.status = 'pending_review'
			ORDER BY f.created_at, f.id LIMIT $2`, p.HospitalID, maxOversightItems)
		if err != nil {
			return err
		}
		for rows.Next() {
			var it OversightItem
			var id uuid.UUID
			var at time.Time
			var assigned, specialty *string
			if err := rows.Scan(&id, &at, &it.SuggestedBy.DisplayName, &specialty, &assigned, &it.EligibleReviewers); err != nil {
				rows.Close()
				return err
			}
			it.ID, it.Ref, it.SubmittedAt = id.String(), Ref(id), stamp(at)
			if specialty != nil && *specialty != "" {
				it.Specialty = specialty
			}
			if assigned != nil {
				it.AssignedTo = &PersonOut{DisplayName: *assigned}
			}
			out.Items = append(out.Items, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT u.specialty, count(*) FROM users u
			WHERE u.hospital_id = $1 AND u.is_active AND u.specialty IS NOT NULL
			  AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'clinician')
			GROUP BY 1 ORDER BY 1 LIMIT $2`, p.HospitalID, maxSpecialties)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s SpecialtyReviewers
			if err := rows.Scan(&s.Specialty, &s.Reviewers); err != nil {
				return err
			}
			out.ReviewersBySpecialty = append(out.ReviewersBySpecialty, s)
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "review oversight", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}

// ReviewerOut is one person who may review a correction.
type ReviewerOut struct {
	ID          string  `json:"id"`
	DisplayName string  `json:"display_name"`
	Specialty   *string `json:"specialty"`
}

// maxReviewers bounds the eligible-reviewer list.
const maxReviewers = 500

// waitingSubmitter is the submitter of a waiting correction of hid
// (errNotFound for none, errDecided when it is no longer waiting).
func waitingSubmitter(ctx context.Context, tx pgx.Tx, hid, id uuid.UUID) (uuid.UUID, error) {
	var by uuid.UUID
	var status string
	err := tx.QueryRow(ctx, `SELECT submitted_by, status FROM feedback_events
		WHERE hospital_id = $1 AND id = $2 AND action = 'correct'`, hid, id).Scan(&by, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return by, errNotFound
	}
	if err != nil {
		return by, err
	}
	if status != "pending_review" {
		return by, errDecided
	}
	return by, nil
}

// eligibleReviewers is GET /hospital/corrections/{id}/eligible-reviewers:
// the active clinicians of the hospital but the submitter.
func (d Deps) eligibleReviewers(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	id, ok := canonicalID(r.PathValue("id"))
	if !ok || r.URL.RawQuery != "" {
		httpx.Error(w, r, "not_found")
		return
	}
	ctx := r.Context()
	out := []ReviewerOut{}
	err := d.tx(ctx, func(tx pgx.Tx) error {
		by, err := waitingSubmitter(ctx, tx, p.HospitalID, id)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT u.id, u.display_name, u.specialty FROM users u
			WHERE u.hospital_id = $1 AND u.is_active AND u.id <> $2
			  AND EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.role = 'clinician')
			ORDER BY u.display_name, u.id LIMIT $3`, p.HospitalID, by, maxReviewers)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x ReviewerOut
			var uid uuid.UUID
			if err := rows.Scan(&uid, &x.DisplayName, &x.Specialty); err != nil {
				return err
			}
			x.ID = uid.String()
			out = append(out, x)
		}
		return rows.Err()
	})
	if err != nil {
		d.fail(w, r, p, "eligible reviewers", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, out)
}
