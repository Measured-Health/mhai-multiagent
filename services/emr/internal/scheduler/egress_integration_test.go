//go:build integration

package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
)

// behindProxy makes the fake connector act as egress-emr would: the fetch
// is refused (unreachable, as the FHIR connector reads a refused CONNECT)
// unless the allow-list file in dir lists the connection.
func (f *fixture) behindProxy(dir string) {
	f.conn.fetch = func(_ context.Context, cur connector.Cursor, _ connector.RawSink) (connector.Cursor, error) {
		l, err := proxyconf.ReadFile(filepath.Join(dir, proxyconf.FileName))
		if err != nil || !l.Fresh(time.Now()) {
			return cur, connector.Fail("unreachable", 0)
		}
		if _, ok := l.Conns[f.id]; !ok {
			return cur, connector.Fail("unreachable", 0)
		}
		return connector.Cursor(`{"n":1}`), nil
	}
}

func (f *fixture) setEnabled(t *testing.T, on bool) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `UPDATE emr_connections SET enabled = $2 WHERE id = $1`, f.id, on); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) nextDue(t *testing.T) time.Time {
	t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT next_due_at FROM emr.sync_state WHERE connection_id = $1`, f.id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// near: the database keeps microseconds.
func near(a, b time.Time) bool { d := a.Sub(b); return d > -time.Millisecond && d < time.Millisecond }

func (f *fixture) counts(t *testing.T) (runs, errs int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM emr.sync_runs WHERE connection_id = $1),
		(SELECT count(*) FROM emr.connection_errors WHERE connection_id = $1)`, f.id).Scan(&runs, &errs); err != nil {
		t.Fatal(err)
	}
	return
}

// The live end-to-end run's bug: a platform admin switches a connection
// on and core's sync_now makes it due at once, before the allow-list's
// next periodic write. The first poll must wait until egress-emr has the
// connection, and succeed.
func TestFirstPollAfterSwitchOnSucceeds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	w := &egress.Writer{Pool: f.pool, Dir: dir, Development: true}
	// Off, and the list written while it was off.
	f.setEnabled(t, false)
	if err := w.WriteOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if l, err := proxyconf.ReadFile(filepath.Join(dir, proxyconf.FileName)); err != nil || len(l.Conns) != 0 {
		t.Fatalf("list before the switch-on: %v %v", l, err)
	}
	f.behindProxy(dir)
	// Switched on, and polled at once (no sync_state row: due now).
	f.setEnabled(t, true)
	s := f.scheduler(f.store)
	s.Egress = w
	start := time.Now()
	n, err := s.Once(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Once: %d %v", n, err)
	}
	if took := time.Since(start); took < egress.ReloadWait-100*time.Millisecond {
		t.Errorf("polled %s after the write: the proxy has had no time to reload", took)
	}
	if outcome, ok, _ := f.lastRun(t); outcome != "ok" || !ok {
		t.Fatalf("first poll after switch-on: %q, want ok", outcome)
	}
	if st := f.state(t); st.failures != 0 {
		t.Errorf("failures %d", st.failures)
	}
	if f.conn.count() != 1 {
		t.Errorf("%d fetches", f.conn.count())
	}
}

// slowList is an allow-list that isn't ready for its first fails calls.
type slowList struct {
	mu    sync.Mutex
	fails int
	calls int
}

func (l *slowList) Ensure(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.fails > 0 {
		l.fails--
		return false, errors.New("allow-list not written")
	}
	return true, nil
}

// An allow-list that isn't ready yet is not the source failing: the poll
// is tried again after NotReadyRetry, with no run, no error row, no
// failure counted and no back-off; then it goes out.
func TestAllowListDelayRetriesShortly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.scheduler(f.store)
	s.Egress = &slowList{fails: 2}
	for i := 1; i <= 2; i++ {
		n, err := s.Once(ctx)
		if err != nil || n != 0 {
			t.Fatalf("put-off %d: Once %d %v", i, n, err)
		}
		if runs, errs := f.counts(t); runs != 0 || errs != 0 {
			t.Fatalf("put-off %d: %d runs, %d error rows; want none", i, runs, errs)
		}
		if st := f.state(t); st.failures != 0 || st.signIn != 0 {
			t.Fatalf("put-off %d: failures %d, sign-in %d", i, st.failures, st.signIn)
		}
		if got, want := f.nextDue(t), f.clock().Add(NotReadyRetry); !near(got, want) {
			t.Fatalf("put-off %d: next due %v, want %v (a short retry, not a back-off)", i, got, want)
		}
		// Not due before the retry.
		if n, _ := s.Once(ctx); n != 0 {
			t.Fatal("polled again before NotReadyRetry")
		}
		f.advance(NotReadyRetry)
	}
	if f.conn.count() != 0 {
		t.Fatalf("%d fetches went out before the list was ready", f.conn.count())
	}
	n, err := s.Once(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Once %d %v", n, err)
	}
	if outcome, _, _ := f.lastRun(t); outcome != "ok" {
		t.Fatalf("outcome %q, want ok", outcome)
	}
	if got := s.Metrics.NotReady.Load(); got != 2 {
		t.Errorf("NotReady %d, want 2", got)
	}
	if runs, errs := f.counts(t); runs != 1 || errs != 0 {
		t.Errorf("%d runs, %d error rows", runs, errs)
	}
}

// The retry is bounded: a list that stays unready is, after MaxNotReady
// put-offs, recorded unreachable and backs off (fail closed). Nothing is
// sent.
func TestAllowListNeverReadyBacksOff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.scheduler(f.store)
	s.Egress = &slowList{fails: 1 << 20}
	for i := 0; i < MaxNotReady; i++ {
		if n, err := s.Once(ctx); err != nil || n != 0 {
			t.Fatalf("put-off %d: %d %v", i, n, err)
		}
		f.advance(NotReadyRetry)
	}
	if n, err := s.Once(ctx); err != nil || n != 1 {
		t.Fatalf("Once %d %v", n, err)
	}
	if outcome, ok, _ := f.lastRun(t); outcome != "unreachable" || ok {
		t.Fatalf("outcome %q", outcome)
	}
	if st := f.state(t); st.failures != 1 {
		t.Errorf("failures %d, want 1", st.failures)
	}
	if got, want := f.nextDue(t), NextDue(f.clock(), 120*time.Second, "unreachable", 1, 0, 0); !near(got, want) {
		t.Errorf("next due %v, want the back-off %v", got, want)
	}
	if f.conn.count() != 0 {
		t.Errorf("%d fetches went out", f.conn.count())
	}
}

// listedList is a ready allow-list.
type listedList struct{ listed bool }

func (l listedList) Ensure(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return l.listed, nil }

// A genuine refusal still fails closed: unreachable, a failure counted and
// the back-off, whether the proxy refuses the host (the connection is on
// the list, its address isn't one it may reach) or the list leaves the
// connection out (a port the proxy never opens: nothing is sent).
func TestBlockedHostStillUnreachable(t *testing.T) {
	check := func(t *testing.T, f *fixture, s *Scheduler, fetches int) {
		t.Helper()
		if n, err := s.Once(context.Background()); err != nil || n != 1 {
			t.Fatalf("Once %d %v", n, err)
		}
		if outcome, ok, _ := f.lastRun(t); outcome != "unreachable" || ok {
			t.Fatalf("outcome %q, want unreachable", outcome)
		}
		if runs, errs := f.counts(t); runs != 1 || errs != 1 {
			t.Errorf("%d runs, %d error rows", runs, errs)
		}
		if st := f.state(t); st.failures != 1 {
			t.Errorf("failures %d, want 1", st.failures)
		}
		if got, want := f.nextDue(t), NextDue(f.clock(), 120*time.Second, "unreachable", 1, 0, 0); !near(got, want) {
			t.Errorf("next due %v, want the back-off %v", got, want)
		}
		if got := s.Metrics.NotReady.Load(); got != 0 {
			t.Errorf("NotReady %d: a refusal is not a list that isn't ready", got)
		}
		if f.conn.count() != fetches {
			t.Errorf("%d fetches, want %d", f.conn.count(), fetches)
		}
	}

	t.Run("proxy refuses the host", func(t *testing.T) {
		f := newFixture(t)
		f.conn.fetch = func(_ context.Context, cur connector.Cursor, _ connector.RawSink) (connector.Cursor, error) {
			return cur, connector.Fail("unreachable", 0) // a 403 address_refused
		}
		s := f.scheduler(f.store)
		s.Egress = listedList{listed: true}
		check(t, f, s, 1)
	})

	t.Run("list leaves it out", func(t *testing.T) {
		f := newFixture(t)
		ctx := context.Background()
		const origin = "https://stub.example.org:8443"
		// An origin changes only while the connection is off.
		f.setEnabled(t, false)
		if _, err := f.pool.Exec(ctx, `UPDATE emr_connections SET base_url = 'https://stub.example.org:8443/api',
			credentials_origin = $2 WHERE id = $1`, f.id, []string{origin}); err != nil {
			t.Fatal(err)
		}
		f.setEnabled(t, true)
		if _, err := f.pool.Exec(ctx, `UPDATE emr.credentials SET origin = $2 WHERE connection_id = $1`, f.id, []string{origin}); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		f.behindProxy(dir)
		s := f.scheduler(f.store)
		s.Egress = &egress.Writer{Pool: f.pool, Dir: dir, Development: true}
		check(t, f, s, 0)
		if l, err := proxyconf.ReadFile(filepath.Join(dir, proxyconf.FileName)); err != nil || len(l.Conns) != 0 {
			t.Errorf("the list after the poll: %v %v; want it empty", l, err)
		}
	})
}
