//go:build integration

package secretsmove

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/secrets"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/testpg"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// Synthetic, obviously fake.
const canary = "synthetic-test-secret-not-real-0005"

// memStore is a Store with OpenBao's version names, in memory; fail makes
// every Put fail.
type memStore struct {
	mu   sync.Mutex
	n    int
	vals map[string]string
	fail bool
}

func (m *memStore) key(p secrets.Path, v string) string { return p.String() + "@" + v }

func (m *memStore) Put(_ context.Context, p secrets.Path, v seal.Secret) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return "", secrets.ErrStore
	}
	m.n++
	ver := fmt.Sprintf("kv-%d", m.n)
	m.vals[m.key(p, ver)] = string(v.Bytes())
	return ver, nil
}
func (m *memStore) Get(context.Context, secrets.Path) (seal.Secret, string, error) {
	return seal.Secret{}, "", secrets.ErrNotFound
}
func (m *memStore) GetVersion(_ context.Context, p secrets.Path, v string) (seal.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.vals[m.key(p, v)]
	if !ok {
		return seal.Secret{}, secrets.ErrNotFound
	}
	return seal.NewSecret([]byte(s)), nil
}
func (m *memStore) DeleteVersion(_ context.Context, p secrets.Path, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vals, m.key(p, v))
	return nil
}
func (m *memStore) DeleteAll(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (m *memStore) DeleteVersionsBefore(context.Context, secrets.Path, time.Duration) error {
	return nil
}
func (m *memStore) Ready(context.Context) error { return nil }

type fixture struct {
	pool   *pgxpool.Pool
	file   *secrets.FileStore
	h, c   uuid.UUID
	paths  []secrets.Path
	before map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	_, pool := testpg.Migrated(t)
	ctx := context.Background()
	f := &fixture{pool: pool, h: uuid.New(), c: uuid.New(), before: map[string]string{}}
	if _, err := pool.Exec(ctx, `INSERT INTO hospitals (id, name, short_code, status) VALUES ($1, 'Synthetic Hospital', $2, 'active')`,
		f.h, "S"+f.h.String()[:6]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO emr_connections (id, hospital_id, type, source_label, base_url, settings, credentials_origin)
		VALUES ($1, $2, 'stub', 'Main record', 'https://stub.example.org/api', '{}', $3)`, f.c, f.h,
		[]string{"https://stub.example.org:443"}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := secrets.NewFileStore(dir, "staging")
	if err != nil {
		t.Fatal(err)
	}
	f.file = file
	// Two fields with rows (one with an older version too), one orphan
	// field with no row.
	for i, field := range []string{"api_key", "client_secret", "orphan_field"} {
		p := secrets.Path{Env: "staging", Hospital: f.h, Connection: f.c, Field: field}
		if i == 0 {
			if _, err := file.Put(ctx, p, seal.NewSecret([]byte(canary+"-old"))); err != nil {
				t.Fatal(err)
			}
		}
		v, err := file.Put(ctx, p, seal.NewSecret([]byte(canary+"-"+field)))
		if err != nil {
			t.Fatal(err)
		}
		if field != "orphan_field" {
			if _, err := pool.Exec(ctx, `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at, secret_version, set_by, origin)
				VALUES ($1, $2, $3, now(), $4, $5, $6)`, f.c, f.h, field, v, uuid.New(), []string{"https://stub.example.org:443"}); err != nil {
				t.Fatal(err)
			}
			f.before[field] = v
			f.paths = append(f.paths, p)
		}
	}
	return f
}

func (f *fixture) version(t *testing.T, field string) string {
	t.Helper()
	var v string
	if err := f.pool.QueryRow(context.Background(), `SELECT secret_version FROM emr.credentials WHERE connection_id = $1 AND field = $2`,
		f.c, field).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The version each row names is moved, the row points at the copy, a
// field with no row is left, and a second run moves nothing.
func TestMoveFromFile(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	to := &memStore{vals: map[string]string{}}
	r, err := FromFile(ctx, f.pool, f.file, to, "production")
	if err != nil || r != (Result{Moved: 2, Orphan: 1}) {
		t.Fatalf("first move: %+v %v", r, err)
	}
	for _, p := range f.paths {
		v := f.version(t, p.Field)
		if !secrets.IsOpenBaoVersion(v) {
			t.Fatalf("%s still names %q", p.Field, v)
		}
		dest := p
		dest.Env = "production"
		got, err := to.GetVersion(ctx, dest, v)
		if err != nil || string(got.Bytes()) != canary+"-"+p.Field {
			t.Fatalf("%s: the copy is not the current value: %v", p.Field, err)
		}
	}
	r, err = FromFile(ctx, f.pool, f.file, to, "production")
	if err != nil || r != (Result{Already: 2, Orphan: 1}) {
		t.Fatalf("second move: %+v %v", r, err)
	}
	if len(to.vals) != 2 {
		t.Fatalf("%d copies, want 2", len(to.vals))
	}
}

// OpenBao refusing: nothing is half moved; the row still names the file
// version, and the error is the closed one.
func TestMoveFromFileStoreDown(t *testing.T) {
	f := newFixture(t)
	to := &memStore{vals: map[string]string{}, fail: true}
	_, err := FromFile(context.Background(), f.pool, f.file, to, "production")
	if !errors.Is(err, ErrDest) {
		t.Fatalf("store down: %v", err)
	}
	for field, v := range f.before {
		if got := f.version(t, field); got != v {
			t.Fatalf("%s changed to %q with the store down", field, got)
		}
	}
	if len(to.vals) != 0 {
		t.Fatal("a copy was left")
	}
}
