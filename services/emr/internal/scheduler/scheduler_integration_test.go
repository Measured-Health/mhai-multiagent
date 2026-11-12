//go:build integration

package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/sink"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// fakeConn is a connector of the stub's type whose Fetch the test writes.
type fakeConn struct {
	mu       sync.Mutex
	calls    int
	inflight int
	most     int
	caught   bool
	rewound  []time.Time
	fetch    func(ctx context.Context, cur connector.Cursor, s connector.RawSink) (connector.Cursor, error)
}

func (f *fakeConn) Type() connector.TypeInfo { t, _ := emrtypes.Lookup("stub"); return t }
func (f *fakeConn) Validate(cfg connector.Config) error {
	return connector.ValidateFields(f.Type(), cfg)
}
func (f *fakeConn) Test(context.Context, connector.Config, connector.Creds) connector.TestResult {
	return connector.Result("ok")
}
func (f *fakeConn) RecordLink(connector.Config, string) (string, bool) { return "", false }
func (f *fakeConn) CaughtUp(connector.Cursor) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caught
}
func (f *fakeConn) Rewind(cur connector.Cursor, since time.Time) connector.Cursor {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rewound = append(f.rewound, since)
	return cur
}
func (f *fakeConn) Fetch(ctx context.Context, _ connector.Config, creds connector.Creds, cur connector.Cursor, s connector.RawSink) (connector.Cursor, error) {
	f.mu.Lock()
	f.calls++
	f.inflight++
	f.most = max(f.most, f.inflight)
	fn := f.fetch
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.inflight--; f.mu.Unlock() }()
	if !creds.Has("api_key") {
		return cur, connector.Fail("sign_in_refused", 0)
	}
	if fn == nil {
		return connector.Cursor(`{"n":1}`), nil
	}
	return fn(ctx, cur, s)
}
func (f *fakeConn) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// downStore is a secret store that is unavailable.
type downStore struct{ secrets.Store }

func (downStore) GetVersion(context.Context, secrets.Path, string) (seal.Secret, error) {
	return seal.Secret{}, secrets.ErrStore
}

type fixture struct {
	pool     *pgxpool.Pool
	store    secrets.Store
	conn     *fakeConn
	hospital uuid.UUID
	id       uuid.UUID
	now      time.Time
	mu       sync.Mutex
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, pool := testpg.Migrated(t)
	ctx := context.Background()
	f := &fixture{pool: pool, conn: &fakeConn{}, hospital: uuid.New(), id: uuid.New(), now: time.Now().UTC()}
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		f.hospital, "S"+f.hospital.String()[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings,
		credentials_origin, enabled, poll_seconds) VALUES ($1, $2, 'stub', 'Main record', 'https://stub.example.org/api',
		'{"client_id": "synthetic-client"}', '{https://stub.example.org:443}', true, 120)`, f.id, f.hospital); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := secrets.NewFileStore(dir, "development")
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	ver, err := store.Put(ctx, secrets.Path{Env: "development", Hospital: f.hospital, Connection: f.id, Field: "api_key"},
		seal.NewSecret([]byte("synthetic-test-secret-not-real-0006")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, origin)
		VALUES ($1, $2, 'api_key', now(), $3, '{https://stub.example.org:443}')`, f.id, f.hospital, ver); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) clock() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fixture) scheduler(store secrets.Store) *Scheduler {
	return &Scheduler{Pool: f.pool, Connectors: connector.NewRegistry(true, f.conn), Store: store, Env: "development",
		NewSink: func(cfg connector.Config, backfill bool) Sink {
			s := sink.New(f.pool, cfg.HospitalID, cfg.ConnectionID, cfg.Type, 1, "", func(context.Context, uuid.UUID, []uuid.UUID) error { return nil })
			s.Backfill = backfill
			return s
		},
		Now: f.clock, Jitter: func() float64 { return 0 }, Metrics: &Metrics{}}
}

// due makes the connection due now.
func (f *fixture) due(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE emr.sync_state SET next_due_at = $2 WHERE connection_id = $1`, f.id, f.clock().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
}

type state struct {
	signIn, failures int
	live             *time.Time
	reconciled       *time.Time
}

func (f *fixture) state(t *testing.T) state {
	t.Helper()
	var s state
	if err := f.pool.QueryRow(context.Background(), `SELECT sign_in_failures, failures, live_since, reconciled_on::timestamptz
		FROM emr.sync_state WHERE connection_id = $1`, f.id).Scan(&s.signIn, &s.failures, &s.live, &s.reconciled); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) lastRun(t *testing.T) (outcome string, ok bool, records int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT outcome, ok, records FROM emr.sync_runs WHERE connection_id = $1
		AND finished_at IS NOT NULL ORDER BY started_at DESC, finished_at DESC LIMIT 1`, f.id).Scan(&outcome, &ok, &records); err != nil {
		t.Fatal(err)
	}
	return
}

func putObs(id string, at time.Time) func(context.Context, connector.Cursor, connector.RawSink) (connector.Cursor, error) {
	return func(ctx context.Context, _ connector.Cursor, s connector.RawSink) (connector.Cursor, error) {
		err := s.Put(ctx, connector.RawRecord{Kind: "observation", SourceID: id, SourceVersion: "1", SourceUpdatedAt: at,
			Fields: map[string]any{"status": "final"}})
		return connector.Cursor(`{"n":2}`), err
	}
}

// Design §7.1: one fetch per connection at a time. Two schedulers (two emr
// copies) see the same due connection; one runs it, the other skips it,
// and once it has run it isn't due again until its next time.
func TestOneFetchPerConnection(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	f.conn.fetch = func(ctx context.Context, cur connector.Cursor, _ connector.RawSink) (connector.Cursor, error) {
		started <- struct{}{}
		<-release
		return connector.Cursor(`{"n":1}`), nil
	}
	a, b := f.scheduler(f.store), f.scheduler(f.store)
	done := make(chan int, 1)
	go func() { n, _ := a.Once(context.Background()); done <- n }()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("the first scheduler never fetched")
	}
	if n, err := b.Once(context.Background()); err != nil || n != 0 {
		t.Fatalf("second scheduler ran %d (%v) while the first held the lock", n, err)
	}
	close(release)
	if n := <-done; n != 1 {
		t.Fatalf("first scheduler ran %d", n)
	}
	if n, _ := b.Once(context.Background()); n != 0 {
		t.Fatalf("ran again before its next time: %d", n)
	}
	if f.conn.count() != 1 || f.conn.most != 1 {
		t.Fatalf("%d fetches, %d at once", f.conn.count(), f.conn.most)
	}
	if out, ok, _ := f.lastRun(t); out != "ok" || !ok {
		t.Fatalf("run %s %v", out, ok)
	}
	// A paused connection, or one of an inactive hospital, isn't polled.
	f.due(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE emr_connections SET paused = true WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.Once(context.Background()); n != 0 {
		t.Fatal("polled a paused connection")
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE emr_connections SET paused = false WHERE id = $1;`, f.id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, f.hospital); err != nil {
		t.Fatal(err)
	}
	if n, _ := a.Once(context.Background()); n != 0 || f.conn.count() != 1 {
		t.Fatal("polled a connection of an inactive hospital")
	}
}

// A18, the emr side: the first sync is a backfill: its records are flagged
// on the raw row, and it ends when the connector has read everything
// (the cursor reached now minus one poll); later records are live.
func TestBackfillThenLive(t *testing.T) {
	f := newFixture(t)
	s := f.scheduler(f.store)
	backfill := func(id string) bool {
		var b bool
		if err := f.pool.QueryRow(context.Background(), `SELECT backfill FROM emr.raw_records WHERE connection_id = $1 AND source_id = $2`,
			f.id, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	f.conn.fetch = putObs("o-1", f.clock().Add(-24*time.Hour))
	if n, err := s.Once(context.Background()); n != 1 || err != nil {
		t.Fatalf("run 1: %d %v", n, err)
	}
	if !backfill("o-1") || f.state(t).live != nil {
		t.Fatal("run 1: not a backfill")
	}
	if _, _, records := f.lastRun(t); records != 1 {
		t.Fatalf("run 1 records %d", records)
	}
	f.conn.mu.Lock()
	f.conn.caught = true
	f.conn.fetch = putObs("o-2", f.clock().Add(-time.Hour))
	f.conn.mu.Unlock()
	f.advance(3 * time.Minute)
	if n, _ := s.Once(context.Background()); n != 1 {
		t.Fatal("run 2 didn't run")
	}
	if !backfill("o-2") || f.state(t).live == nil {
		t.Fatal("run 2: the catching-up run is still backfill, and ends it")
	}
	f.conn.mu.Lock()
	f.conn.fetch = putObs("o-3", f.clock())
	f.conn.mu.Unlock()
	f.advance(3 * time.Minute)
	if n, _ := s.Once(context.Background()); n != 1 {
		t.Fatal("run 3 didn't run")
	}
	if backfill("o-3") {
		t.Fatal("run 3: a live record flagged backfill")
	}
}

// Design §15.2: the secret store down is unreachable, never
// sign_in_refused, and nothing is fetched.
func TestSecretStoreDownIsUnreachable(t *testing.T) {
	f := newFixture(t)
	s := f.scheduler(downStore{f.store})
	if n, _ := s.Once(context.Background()); n != 1 {
		t.Fatal("didn't run")
	}
	if out, ok, _ := f.lastRun(t); out != "unreachable" || ok {
		t.Fatalf("outcome %s %v", out, ok)
	}
	var code string
	if err := f.pool.QueryRow(context.Background(), `SELECT code FROM emr.connection_errors WHERE connection_id = $1`, f.id).Scan(&code); err != nil || code != "unreachable" {
		t.Fatalf("error row %q %v", code, err)
	}
	if st := f.state(t); st.signIn != 0 || st.failures != 1 || f.conn.count() != 0 {
		t.Fatalf("state %+v, %d fetches", st, f.conn.count())
	}
}

// Design §15.2: after 3 sign-in refusals in a row the connection is not
// polled again until a test passes (Resume).
func TestThreeSignInFailuresStopPolling(t *testing.T) {
	f := newFixture(t)
	f.conn.fetch = func(context.Context, connector.Cursor, connector.RawSink) (connector.Cursor, error) {
		return nil, connector.Fail("sign_in_refused", 0)
	}
	s := f.scheduler(f.store)
	for i := 1; i <= 3; i++ {
		if n, _ := s.Once(context.Background()); n != 1 {
			t.Fatalf("run %d didn't run", i)
		}
		if st := f.state(t); st.signIn != i {
			t.Fatalf("after %d: sign-in failures %d", i, st.signIn)
		}
		f.due(t)
	}
	if n, _ := s.Once(context.Background()); n != 0 || f.conn.count() != 3 {
		t.Fatalf("polled after 3 sign-in refusals: %d, %d fetches", n, f.conn.count())
	}
	if out, _, _ := f.lastRun(t); out != "sign_in_refused" {
		t.Fatalf("outcome %s", out)
	}
	f.conn.mu.Lock()
	f.conn.fetch = nil
	f.conn.mu.Unlock()
	if err := Resume(context.Background(), f.pool, f.hospital, f.id, f.clock()); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Once(context.Background()); n != 1 || f.state(t).signIn != 0 {
		t.Fatal("not polled after Resume")
	}
	// The origin the saved addresses give no longer matches the
	// credentials': sign_in_refused, nothing fetched.
	if _, err := f.pool.Exec(context.Background(), `UPDATE emr_connections SET base_url = 'https://moved.example.org/api' WHERE id = $1`, f.id); err != nil {
		t.Fatal(err)
	}
	f.due(t)
	before := f.conn.count()
	if n, _ := s.Once(context.Background()); n != 1 || f.conn.count() != before {
		t.Fatalf("moved origin fetched: %d", f.conn.count()-before)
	}
	if out, _, _ := f.lastRun(t); out != "sign_in_refused" {
		t.Fatalf("moved origin outcome %s", out)
	}
}

// A20: a 429's Retry-After holds the next request off on the clock.
func TestRetryAfterHonoured(t *testing.T) {
	f := newFixture(t)
	f.conn.fetch = func(context.Context, connector.Cursor, connector.RawSink) (connector.Cursor, error) {
		return nil, connector.Fail("too_many_requests", 120*time.Second)
	}
	s := f.scheduler(f.store)
	if n, _ := s.Once(context.Background()); n != 1 {
		t.Fatal("didn't run")
	}
	f.advance(119 * time.Second)
	if n, _ := s.Once(context.Background()); n != 0 {
		t.Fatal("asked again within the Retry-After")
	}
	f.advance(2 * time.Second)
	if n, _ := s.Once(context.Background()); n != 1 || f.conn.count() != 2 {
		t.Fatalf("not asked after the Retry-After: %d", f.conn.count())
	}
}

// Design §5.1: once a day, from 02:00 hospital time, the last 24 hours are
// read again; a record found only then is counted as reconciled late.
func TestDailyReconciliation(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pool.Exec(context.Background(), `UPDATE hospitals SET time_zone = 'Africa/Lagos' WHERE id = $1`, f.hospital); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.now = time.Date(2026, 11, 12, 0, 30, 0, 0, time.UTC) // 01:30 in Lagos
	f.mu.Unlock()
	f.conn.caught = true
	s := f.scheduler(f.store)
	if n, _ := s.Once(context.Background()); n != 1 || f.state(t).live == nil {
		t.Fatal("first run")
	}
	f.conn.mu.Lock()
	rewound := len(f.conn.rewound)
	f.conn.mu.Unlock()
	if rewound != 0 {
		t.Fatal("reconciled before 02:00, and during the backfill")
	}
	f.advance(time.Hour) // 02:30 in Lagos
	f.due(t)
	f.conn.mu.Lock()
	f.conn.fetch = putObs("o-late", f.clock().Add(-3*time.Hour))
	f.conn.mu.Unlock()
	if n, _ := s.Once(context.Background()); n != 1 {
		t.Fatal("reconciliation run")
	}
	f.conn.mu.Lock()
	got := append([]time.Time(nil), f.conn.rewound...)
	f.conn.mu.Unlock()
	if len(got) != 1 || !got[0].Equal(f.clock().Add(-24*time.Hour)) {
		t.Fatalf("rewound %v", got)
	}
	if s.Metrics.ReconciledLate.Load() != 1 {
		t.Fatalf("reconciled late %d", s.Metrics.ReconciledLate.Load())
	}
	if st := f.state(t); st.reconciled == nil {
		t.Fatal("not marked reconciled")
	}
	f.advance(3 * time.Minute)
	f.due(t)
	if n, _ := s.Once(context.Background()); n != 1 {
		t.Fatal("next run")
	}
	f.conn.mu.Lock()
	defer f.conn.mu.Unlock()
	if len(f.conn.rewound) != 1 {
		t.Fatal("reconciled twice in a day")
	}
}

var _ = errors.New
