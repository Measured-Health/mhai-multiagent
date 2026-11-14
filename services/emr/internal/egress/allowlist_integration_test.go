//go:build integration

package egress

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/proxyconf"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
)

func seedHospital(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO hospitals (id, name, short_code, status)
		VALUES ($1, 'Synthetic Hospital', $2, 'active')`, id, "S"+id.String()[:6]); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedConn adds a connection as the owner would through core: its stored
// origin is the one its addresses give unless origin says otherwise.
func seedConn(t *testing.T, pool *pgxpool.Pool, hospital uuid.UUID, typ, base string, origin []string, enabled bool, cidrs []string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if cidrs == nil {
		cidrs = []string{}
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO emr_connections (id, hospital_id, type, source_label,
		base_url, credentials_origin, enabled, private_network, private_cidrs)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::cidr[])`,
		id, hospital, typ, "Source "+id.String()[:8], base, origin, enabled, len(cidrs) > 0, cidrs); err != nil {
		t.Fatal(err)
	}
	return id
}

func readList(t *testing.T, dir string) *proxyconf.List {
	t.Helper()
	l, err := proxyconf.ReadFile(filepath.Join(dir, proxyconf.FileName))
	if err != nil {
		return nil
	}
	return l
}

// waitFor polls the file until ok holds, and returns how long it took.
func waitFor(t *testing.T, dir string, limit time.Duration, ok func(*proxyconf.List) bool) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < limit {
		if l := readList(t, dir); l != nil && ok(l) {
			return time.Since(start)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("not within %s", limit)
	return 0
}

// Design §6.4, departure 15: the emr service writes allow.json from the
// enabled connections (not paused, origin unchanged since the credentials
// were entered) and those under test, with each one's hosts and declared
// ranges, and a change reaches the file within 30 s. The file is one the
// proxy accepts, written atomically, readable by the proxy only through
// its read-only mount.
func TestAllowListWithin30s(t *testing.T) {
	_, pool := testpg.Migrated(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ha, hb := seedHospital(t, pool), seedHospital(t, pool)
	a := seedConn(t, pool, ha, "fhir_r4", "https://fhir.hospital-a.example/r4", []string{"https://fhir.hospital-a.example:443"},
		true, []string{"10.99.1.0/24"})
	sftp := seedConn(t, pool, ha, "file_export", "sftp://sftp.hospital-a.example/out", []string{"sftp://sftp.hospital-a.example:22"},
		true, nil)
	b := seedConn(t, pool, hb, "fhir_r4", "https://fhir.hospital-b.example/r4", []string{"https://fhir.hospital-b.example:443"},
		false, []string{"10.99.2.0/24"})
	// Enabled, but the stored origin isn't the one the address gives:
	// never listed (its credentials were entered for another host).
	moved := seedConn(t, pool, hb, "fhir_r4", "https://moved.hospital-b.example/r4", []string{"https://fhir.hospital-b.example:443"},
		true, nil)
	// Enabled on a port the proxy never opens: never listed.
	port := seedConn(t, pool, hb, "fhir_r4", "https://fhir.hospital-b.example:8443/r4", []string{"https://fhir.hospital-b.example:8443"},
		true, nil)
	paused := seedConn(t, pool, hb, "stub", "https://stub.hospital-b.example/api", []string{"https://stub.hospital-b.example:443"},
		true, nil)
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET paused = true WHERE id = $1`, paused); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	w := &Writer{Pool: pool, Dir: dir}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitFor(t, dir, 30*time.Second, func(l *proxyconf.List) bool { return len(l.Conns) > 0 })
	l := readList(t, dir)
	if !l.Fresh(time.Now()) {
		t.Fatalf("written at %v: not fresh", l.GeneratedAt)
	}
	if len(l.Conns) != 2 {
		t.Fatalf("listed %v, want A and A's sftp only", l.Conns)
	}
	ea := l.Conns[a]
	if ea.HospitalID != ha || !slices.Equal(ea.Hosts, []string{"fhir.hospital-a.example:443"}) ||
		len(ea.CIDRs) != 1 || ea.CIDRs[0].String() != "10.99.1.0/24" {
		t.Errorf("A: %+v", ea)
	}
	if es := l.Conns[sftp]; !slices.Equal(es.Hosts, []string{"sftp.hospital-a.example:22"}) {
		t.Errorf("sftp: %+v", es)
	}
	for _, id := range []uuid.UUID{b, moved, port, paused} {
		if _, ok := l.Conns[id]; ok {
			t.Errorf("%s listed", id)
		}
	}
	st, err := os.Stat(filepath.Join(dir, proxyconf.FileName))
	if err != nil || st.Mode().Perm()&0o022 != 0 {
		t.Errorf("allow.json mode %v (%v): never group or world writable", st.Mode(), err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("leftover files in the directory: %v", ents)
	}

	// Switch B on and A off: both within 30 s.
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = (id = $1) WHERE id IN ($1, $2)`, b, a); err != nil {
		t.Fatal(err)
	}
	took := waitFor(t, dir, 30*time.Second, func(l *proxyconf.List) bool {
		_, hasB := l.Conns[b]
		_, hasA := l.Conns[a]
		return hasB && !hasA
	})
	t.Logf("a change reached allow.json in %s", took)
	if e := readList(t, dir).Conns[b]; e.HospitalID != hb || e.CIDRs[0].String() != "10.99.2.0/24" {
		t.Errorf("B: %+v", e)
	}

	// A connection under test is listed at once while held, through its
	// own hospital only, and unlisted after.
	release, err := w.Hold(ctx, ha, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readList(t, dir).Conns[a]; !ok {
		t.Error("a held connection isn't listed when Hold returns")
	}
	releaseWrong, err := w.Hold(ctx, hb, a) // A's id under B's hospital: RLS finds nothing
	if err != nil {
		t.Fatal(err)
	}
	release()
	waitFor(t, dir, 30*time.Second, func(l *proxyconf.List) bool { _, ok := l.Conns[a]; return !ok })
	releaseWrong()
	if age, ok := w.Age(time.Now()); !ok || age > 30*time.Second {
		t.Errorf("age %s %v", age, ok)
	}

	// The database gone: the file is never rewritten, so it ages out and
	// the proxy refuses everything (fail closed).
	before := readList(t, dir).GeneratedAt
	pool.Close()
	time.Sleep(Interval + 2*time.Second)
	if after := readList(t, dir).GeneratedAt; !after.Equal(before) {
		t.Error("allow.json rewritten without a database")
	}
	cancel()
	<-done
}

// Ensure: the scheduler's check before a poll. A connection switched on
// after the last write is listed by a write made at once, and Ensure
// returns only once the proxy has had time to read it; a second call
// doesn't wait again. Ensure adds nothing to the list: a connection that
// is off, one on a port the proxy never opens, or an id under another
// hospital is not listed. Without a database the list isn't ready (an
// error), never "listed".
func TestEnsureListsSwitchedOnAtOnce(t *testing.T) {
	_, pool := testpg.Migrated(t)
	ctx := context.Background()
	ha, hb := seedHospital(t, pool), seedHospital(t, pool)
	a := seedConn(t, pool, ha, "fhir_r4", "https://fhir.hospital-a.example/r4", []string{"https://fhir.hospital-a.example:443"},
		false, nil)
	port := seedConn(t, pool, ha, "fhir_r4", "https://fhir.hospital-a.example:8443/r4", []string{"https://fhir.hospital-a.example:8443"},
		true, nil)
	off := seedConn(t, pool, hb, "fhir_r4", "https://fhir.hospital-b.example/r4", []string{"https://fhir.hospital-b.example:443"},
		false, nil)
	dir := t.TempDir()
	w := &Writer{Pool: pool, Dir: dir}
	if err := w.WriteOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if l := readList(t, dir); l == nil || len(l.Conns) != 0 {
		t.Fatalf("before the switch-on: %v", l)
	}
	if _, err := pool.Exec(ctx, `UPDATE emr_connections SET enabled = true WHERE id = $1`, a); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	listed, err := w.Ensure(ctx, ha, a)
	if err != nil || !listed {
		t.Fatalf("Ensure: %v %v", listed, err)
	}
	if took := time.Since(start); took < ReloadWait-100*time.Millisecond {
		t.Errorf("returned after %s: before the proxy's reload", took)
	}
	if _, ok := readList(t, dir).Conns[a]; !ok {
		t.Fatal("not in the file when Ensure returned")
	}
	start = time.Now()
	if listed, err := w.Ensure(ctx, ha, a); err != nil || !listed {
		t.Fatalf("second Ensure: %v %v", listed, err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("second Ensure waited %s", took)
	}
	for _, c := range []struct {
		name     string
		hospital uuid.UUID
		id       uuid.UUID
	}{{"off", hb, off}, {"port 8443", ha, port}, {"another hospital's id", hb, a}} {
		if listed, err := w.Ensure(ctx, c.hospital, c.id); err != nil || listed {
			t.Errorf("%s: listed %v, err %v; want false, nil", c.name, listed, err)
		}
	}
	l := readList(t, dir)
	if len(l.Conns) != 1 {
		t.Errorf("listed %v, want A only", l.Conns)
	}
	pool.Close()
	if listed, err := w.Ensure(ctx, hb, off); err == nil || listed {
		t.Errorf("without a database: listed %v, err %v; want an error", listed, err)
	}
}
