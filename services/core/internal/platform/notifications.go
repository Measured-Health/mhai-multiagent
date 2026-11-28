package platform

// The platform notifications (mhai-web Plan 7d; frontend requirements
// §14.7; mhai-web's notifications-contract.ts):
//
//   - GET /platform/notifications: platform admins and auditors; at most
//     NotificationsMax items, and unseen, the count of ALL the caller's
//     unseen ones.
//   - POST /platform/notifications/seen {ids?}: platform admins (core's
//     write rule since 1.26.0; an auditor is 403); marks the given ones
//     seen for the caller, or every current one when ids is left out. An
//     id that isn't a current notification is ignored. Answers {unseen}.
//     Not audited: it is the caller's own read state, no admin action (as
//     mhai-web's sample API).
//
// What produces them (nothing else does): the audit log's newest check
// when it came back broken (kind audit_integrity: the run, its first
// broken entry's seq and id), first; then What changed's open changes
// (kind analytics_change, one per change of the newest weekly evaluation,
// exactly as GET /platform/analytics sends them, newest flagged first).
// Counts, codes, ids and hospital names only: never a patient, a person or
// any clinical text. A stored change that can't be sent is left out (and
// logged), never shown in part. Seen is per person (00062), by the
// notification's id.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const (
	// NotificationsMax is the most items GET sends (mhai-web
	// NOTIFICATIONS_MAX).
	NotificationsMax = 20
	// maxSeenIDs bounds POST seen's ids.
	maxSeenIDs = 200
)

// notificationID is a notification's id as this build makes them.
var notificationID = regexp.MustCompile(`^ntf_(audit_)?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func registerNotifications(m *authz.Mux, d Deps) {
	read := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: TeamRoles}
	write := authz.Rule{Platform: true, NeedsMFA: true, AnyRole: []string{authz.RolePlatformAdmin}, Write: true}
	authz.Route(m, http.MethodGet, "/platform/notifications", read, http.HandlerFunc(d.notificationsList))
	authz.Route(m, http.MethodPost, "/platform/notifications/seen", write, http.HandlerFunc(d.notificationsSeen))
}

// Notification is one item: an analytics change or a broken audit check.
// Only the kind's own fields are sent (omitempty on the others).
type Notification struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// analytics_change
	ChangeID  string          `json:"change_id,omitempty"`
	Hospital  *analytics.Ref  `json:"-"`
	Summary   string          `json:"summary,omitempty"`
	Serious   *bool           `json:"serious,omitempty"`
	Direction string          `json:"direction,omitempty"`
	Measure   string          `json:"measure,omitempty"`
	Previous  json.RawMessage `json:"previous,omitempty"`
	Current   json.RawMessage `json:"current,omitempty"`
	// audit_integrity
	RunID          string  `json:"run_id,omitempty"`
	FirstBrokenSeq *int64  `json:"first_broken_seq,omitempty"`
	FirstBrokenID  *string `json:"first_broken_id,omitempty"`

	At   string `json:"at"`
	Seen bool   `json:"seen"`
}

// MarshalJSON sends hospital (null for a change across all hospitals) on a
// change only.
func (n Notification) MarshalJSON() ([]byte, error) {
	type plain Notification
	if n.Kind != "analytics_change" {
		return json.Marshal(plain(n))
	}
	return json.Marshal(struct {
		plain
		Hospital *analytics.Ref `json:"hospital"`
	}{plain(n), n.Hospital})
}

// NotificationsOut is GET's reply.
type NotificationsOut struct {
	Items  []Notification `json:"items"`
	Unseen int            `json:"unseen"`
}

// currentNotifications reads every current notification as you see it,
// in the order sent.
func (d Deps) currentNotifications(ctx context.Context, tx pgx.Tx, r *http.Request, you uuid.UUID) ([]Notification, error) {
	out := []Notification{}
	// The newest finished check, when it came back broken.
	var run uuid.UUID
	var state string
	var seq *int64
	var first *string
	var at time.Time
	err := tx.QueryRow(ctx, `SELECT id, state, first_broken_seq, first_broken_id, COALESCE(finished_at, started_at)
		FROM audit_verify_runs WHERE state <> 'running' ORDER BY started_at DESC, id DESC LIMIT 1`).
		Scan(&run, &state, &seq, &first, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	case state == "broken" && seq != nil && first != nil:
		out = append(out, Notification{Kind: "audit_integrity", ID: "ntf_audit_" + run.String(), RunID: run.String(),
			FirstBrokenSeq: seq, FirstBrokenID: first, At: stamp(at)})
	}

	// What changed's open changes, as the analytics route reads them.
	cr, changes, err := openChangesIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	if cr != nil && len(changes) > 0 {
		ids := make([]uuid.UUID, 0, len(changes))
		for _, c := range changes {
			ids = append(ids, uuid.MustParse(c.ID))
		}
		flagged := map[string]time.Time{}
		rows, err := tx.Query(ctx, `SELECT c.id, COALESCE(
				(SELECT r.evaluated_at FROM analytics_change_runs r WHERE r.week_from = c.opened_week), $2)
			FROM analytics_changes c WHERE c.id = ANY($1)`, ids, cr.EvaluatedAt)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id uuid.UUID
			var t time.Time
			if err := rows.Scan(&id, &t); err != nil {
				rows.Close()
				return nil, err
			}
			flagged[id.String()] = t
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		var items []Notification
		for _, c := range changes {
			o, err := analytics.ChangeOutOf(c, "")
			if err != nil {
				d.log().Warn("platform: notifications left out a stored change", "request_id", httpx.RequestIDFrom(r.Context()),
					"reason", "unreadable_change")
				continue
			}
			serious := o.Serious
			items = append(items, Notification{Kind: "analytics_change", ID: "ntf_" + o.ID, ChangeID: o.ID, Hospital: o.Hospital,
				Summary: o.Summary, Serious: &serious, Direction: o.Direction, Measure: o.Measure, Previous: o.Previous,
				Current: o.Current, At: stamp(flagged[o.ID])})
		}
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].At != items[j].At {
				return items[i].At > items[j].At
			}
			return items[i].ID < items[j].ID
		})
		out = append(out, items...)
	}

	// Seen, by the caller.
	rows, err := tx.Query(ctx, `SELECT notification_id FROM platform_notifications_seen WHERE user_id = $1`, you)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Seen = seen[out[i].ID]
	}
	return out, nil
}

func unseenOf(items []Notification) int {
	n := 0
	for _, i := range items {
		if !i.Seen {
			n++
		}
	}
	return n
}

func (d Deps) notificationsList(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var all []Notification
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		var err error
		all, err = d.currentNotifications(r.Context(), tx, r, p.UserID)
		return err
	})
	if err != nil {
		d.fail(w, r, p, "notifications read")
		return
	}
	items := all
	if len(items) > NotificationsMax {
		items = items[:NotificationsMax]
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, NotificationsOut{Items: items, Unseen: unseenOf(all)})
}

func (d Deps) notificationsSeen(w http.ResponseWriter, r *http.Request) {
	p, ok := caller(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var b struct {
		IDs *[]string `json:"ids"`
	}
	if !httpx.Decode(w, r, &b) {
		return
	}
	if b.IDs != nil && len(*b.IDs) > maxSeenIDs {
		httpx.Error(w, r, "validation_failed")
		return
	}
	var unseen int
	err := d.tx(r.Context(), func(tx pgx.Tx) error {
		all, err := d.currentNotifications(r.Context(), tx, r, p.UserID)
		if err != nil {
			return err
		}
		var mark []string
		for i, n := range all {
			if n.Seen || !notificationID.MatchString(n.ID) || (b.IDs != nil && !slices.Contains(*b.IDs, n.ID)) {
				continue
			}
			mark = append(mark, n.ID)
			all[i].Seen = true
		}
		if len(mark) > 0 {
			if _, err := tx.Exec(r.Context(), `INSERT INTO platform_notifications_seen (user_id, notification_id)
				SELECT $1, x FROM unnest($2::text[]) x ON CONFLICT DO NOTHING`, p.UserID, mark); err != nil {
				return err
			}
		}
		unseen = unseenOf(all)
		return nil
	})
	if err != nil {
		d.fail(w, r, p, "notifications seen")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"unseen": unseen})
}
