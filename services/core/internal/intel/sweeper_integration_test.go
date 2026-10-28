//go:build integration

package intel

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
)

// seedQuery adds a clinical query row as the superuser: queued, created
// age ago, with deadline (nil: none) and published (false: never).
func seedQuery(t *testing.T, hid, createdBy uuid.UUID, age time.Duration, deadline *time.Time, published bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var pub any
	if published {
		pub = time.Now()
	}
	var by any
	if createdBy != uuid.Nil {
		by = createdBy
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO clinical_queries
		(id, hospital_id, clinical_question, urgency, status, created_by, created_at, deadline_at, published_at, request_id)
		VALUES ($1, $2, 'synthetic question', 'routine', 'queued', $3, now() - make_interval(secs => $4), $5, $6, $7)`,
		id, hid, by, age.Seconds(), deadline, pub, "rid-"+id.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

type queryState struct {
	status       string
	errCode      *string
	cancelReason *string
	published    bool
	completed    bool
}

func stateOf(t *testing.T, id uuid.UUID) queryState {
	t.Helper()
	var s queryState
	if err := super.QueryRow(context.Background(), `SELECT status, error, cancel_reason, published_at IS NOT NULL, completed_at IS NOT NULL
		FROM clinical_queries WHERE id = $1`, id).Scan(&s.status, &s.errCode, &s.cancelReason, &s.published, &s.completed); err != nil {
		t.Fatal(err)
	}
	return s
}

type auditRow struct {
	actor, hospital, requestID *string
	entity, entityID           *string
	payload                    map[string]any
}

func auditFor(t *testing.T, action, entityID string) []auditRow {
	t.Helper()
	return auditQuery(t, `entity_id`, action, entityID)
}

// auditQuery reads the audit rows with action whose column col is val. col
// is a literal from the callers above, never input.
func auditQuery(t *testing.T, col, action, val string) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT actor_id::text, hospital_id::text, request_id, entity, entity_id, payload
		FROM audit_log WHERE action = $1 AND `+col+` = $2 ORDER BY id`, action, val)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var raw []byte
		if err := rows.Scan(&r.actor, &r.hospital, &r.requestID, &r.entity, &r.entityID, &raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(raw, &r.payload)
		out = append(out, r)
	}
	return out
}

// The sweeper publishes a queued row that was never published, once, with a
// fresh token minted as its creator with the creator's current roles; it
// leaves young and already-published rows alone; a row whose creator can no
// longer use the service is cancelled, not published.
func TestSweeperRepublishesUnpublished(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician, authz.RoleAnalyst)
	gone := seedUser(t, hid, false, authz.RoleClinician)
	analyst := seedUser(t, hid, true, authz.RoleAnalyst)
	deadline := time.Now().Truncate(time.Second).Add(5 * time.Minute)

	stale := seedQuery(t, hid, creator, 20*time.Second, &deadline, false)
	young := seedQuery(t, hid, creator, 0, &deadline, false)
	done := seedQuery(t, hid, creator, 20*time.Second, &deadline, true)
	inactive := seedQuery(t, hid, gone, 20*time.Second, &deadline, false)
	noRole := seedQuery(t, hid, analyst, 20*time.Second, &deadline, false)
	orphan := seedQuery(t, hid, uuid.Nil, 20*time.Second, &deadline, false)

	if err := SweepOnce(ctx, app, c); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	msgs := jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")
	if len(msgs) != 1 {
		t.Fatalf("%d jobs published, want 1", len(msgs))
	}
	m := msgs[0]
	if m.header.Get("Nats-Msg-Id") != stale.String() {
		t.Fatalf("published %s, want %s", m.header.Get("Nats-Msg-Id"), stale)
	}
	cl := verifyJob(t, m)
	roles := slices.Clone(cl.Roles)
	slices.Sort(roles)
	if cl.Sub != creator.String() || cl.Hid != hid.String() || cl.Rid != "rid-"+stale.String() ||
		!slices.Equal(roles, []string{authz.RoleAnalyst, authz.RoleClinician}) || !cl.ExpiresAt.Equal(deadline) {
		t.Fatalf("claims %+v", cl)
	}
	if time.Since(cl.IssuedAt) > 10*time.Second {
		t.Fatalf("token not fresh: iat %v", cl.IssuedAt)
	}
	if s := stateOf(t, stale); s.status != "queued" || !s.published {
		t.Fatalf("stale row %+v", s)
	}
	if s := stateOf(t, young); s.published || s.status != "queued" {
		t.Fatalf("young row touched: %+v", s)
	}
	if s := stateOf(t, done); s.status != "queued" {
		t.Fatalf("published row touched: %+v", s)
	}
	for name, id := range map[string]uuid.UUID{"inactive creator": inactive, "creator without the role": noRole, "no creator": orphan} {
		s := stateOf(t, id)
		if s.status != "cancelled" || s.cancelReason == nil || *s.cancelReason != "account_unusable" || !s.completed || s.published {
			t.Errorf("%s: %+v", name, s)
		}
		a := auditFor(t, "query.cancelled", id.String())
		if len(a) != 1 || *a[0].hospital != hid.String() || a[0].actor != nil || *a[0].requestID != "rid-"+id.String() ||
			a[0].payload["query_id"] != id.String() || a[0].payload["reason"] != "account_unusable" {
			t.Errorf("%s audit: %+v", name, a)
		}
	}

	// Once published, a second sweep sends nothing.
	if err := SweepOnce(ctx, app, c); err != nil {
		t.Fatal(err)
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")); n != 0 {
		t.Fatalf("second sweep published %d", n)
	}
}

// A creator whose hospital stopped running is unusable too.
func TestSweeperCancelsForStoppedHospital(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, creator, 20*time.Second, &deadline, false)
	if _, err := super.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, hid); err != nil {
		t.Fatal(err)
	}
	if err := SweepOnce(ctx, app, c); err != nil {
		t.Fatal(err)
	}
	if s := stateOf(t, q); s.status != "cancelled" {
		t.Fatalf("%+v", s)
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")); n != 0 {
		t.Fatalf("published %d", n)
	}
}

// Past its deadline, an unpublished row fails with deadline_exceeded and
// is audited, and nothing is published. A row with no deadline takes the
// operation's (created_at + 10 minutes).
func TestSweeperFailsPastDeadline(t *testing.T) {
	c, _ := newClient(t)
	ctx := context.Background()
	drainJobs(t, sharedNATS.PipelineURL)
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	past := time.Now().Add(-time.Minute)
	late := seedQuery(t, hid, creator, 20*time.Minute, &past, false)
	noDeadline := seedQuery(t, hid, creator, 11*time.Minute, nil, false)
	if err := SweepOnce(ctx, app, c); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{late, noDeadline} {
		s := stateOf(t, id)
		if s.status != "failed" || s.errCode == nil || *s.errCode != "deadline_exceeded" || !s.completed || s.published {
			t.Fatalf("%s: %+v", id, s)
		}
		a := auditFor(t, "query.failed", id.String())
		if len(a) != 1 || *a[0].hospital != hid.String() || a[0].payload["error"] != "deadline_exceeded" ||
			a[0].payload["query_id"] != id.String() || a[0].payload["attempts"] != float64(0) {
			t.Fatalf("audit %+v", a)
		}
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")); n != 0 {
		t.Fatalf("published %d", n)
	}
}

// RunPublishSweeper sweeps on its ticker until its context ends.
func TestRunPublishSweeperTicks(t *testing.T) {
	c, _ := newClient(t)
	hid := seedHospital(t, "active")
	creator := seedUser(t, hid, true, authz.RoleClinician)
	deadline := time.Now().Add(5 * time.Minute)
	q := seedQuery(t, hid, creator, 20*time.Second, &deadline, false)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); RunPublishSweeper(ctx, app, c, 50*time.Millisecond) }()
	end := time.Now().Add(10 * time.Second)
	for !stateOf(t, q).published {
		if time.Now().After(end) {
			t.Fatal("never swept")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	drainJobs(t, sharedNATS.PipelineURL)
}

// Republish is the one re-mint helper (F23): it refuses an unusable
// creator with ErrCreatorUnusable and publishes nothing.
func TestRepublishRefusesUnusableCreator(t *testing.T) {
	c, _ := newClient(t)
	hid := seedHospital(t, "active")
	gone := seedUser(t, hid, false, authz.RoleClinician)
	other := seedHospital(t, "active")
	elsewhere := seedUser(t, other, true, authz.RoleClinician)
	for name, by := range map[string]uuid.UUID{"inactive": gone, "another hospital's user": elsewhere, "unknown": uuid.New()} {
		row := JobRow{ID: uuid.New(), HospitalID: hid, CreatedBy: by, RequestID: "rid", Deadline: time.Now().Add(time.Minute)}
		if err := Republish(context.Background(), app, c, row, row.ID); !errors.Is(err, ErrCreatorUnusable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(jobsFor(drainJobs(t, sharedNATS.PipelineURL), "jobs.v1."+hid.String()+".run_query")); n != 0 {
		t.Fatalf("published %d", n)
	}
}

// A19: with a limit of 2, five parallel reservations for hospital A succeed
// exactly twice; hospital B still reserves. Rows that are done don't count.
func TestInFlightCapPerHospital(t *testing.T) {
	ctx := context.Background()
	a, b := seedHospital(t, "active"), seedHospital(t, "active")
	// A finished query doesn't hold a slot.
	if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status)
		VALUES ($1, $2, 'synthetic', 'routine', 'completed')`, uuid.New(), a); err != nil {
		t.Fatal(err)
	}
	reserve := func(hid uuid.UUID) error {
		return db.WithHospital(ctx, app, hid, func(tx pgx.Tx) error {
			if err := ReserveInFlight(ctx, tx, hid, 2); err != nil {
				return err
			}
			// Hold the slot a moment, as a real submit's insert would.
			time.Sleep(50 * time.Millisecond)
			_, err := tx.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) VALUES ($1, $2, 'synthetic', 'routine')`,
				uuid.New(), hid)
			return err
		})
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = reserve(a)
		}(i)
	}
	close(start)
	wg.Wait()
	ok, capped := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrAtCap):
			capped++
		default:
			t.Fatalf("reserve: %v", err)
		}
	}
	if ok != 2 || capped != 3 {
		t.Fatalf("%d reserved, %d at the cap; want 2 and 3", ok, capped)
	}
	if code, _ := Code(ErrAtCap); code != "rate_limited" {
		t.Fatalf("code %s", code)
	}
	if err := reserve(b); err != nil {
		t.Fatalf("hospital B: %v", err)
	}
	// running counts too.
	if _, err := super.Exec(ctx, `UPDATE clinical_queries SET status = 'running' WHERE hospital_id = $1 AND status = 'queued'`, a); err != nil {
		t.Fatal(err)
	}
	if err := reserve(a); !errors.Is(err, ErrAtCap) {
		t.Fatalf("running rows don't count: %v", err)
	}
	// The hospital must be the transaction's own: a reservation for
	// another hospital (whose rows RLS hides) is refused, not free.
	err := db.WithHospital(ctx, app, b, func(tx pgx.Tx) error { return ReserveInFlight(ctx, tx, a, 2) })
	if err == nil || errors.Is(err, ErrAtCap) {
		t.Fatalf("cross-hospital reservation: %v", err)
	}
	// A limit under one is refused.
	if err := db.WithHospital(ctx, app, b, func(tx pgx.Tx) error { return ReserveInFlight(ctx, tx, b, 0) }); err == nil {
		t.Fatal("limit 0 accepted")
	}
}
