//go:build integration

package httpx_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// super is a superuser pool for setup and inspection; app connects as a
// login that is only a member of mhai_app, like the gateway.
var super, app *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	pg, stop, err := testutil.NewPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		os.Exit(1)
	}
	code := func() int {
		defer stop()
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "migrate up:", err)
			return 1
		}
		if super, err = db.Open(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			return 1
		}
		defer super.Close()
		if app, err = testutil.AppLogin(ctx, pg, super); err != nil {
			fmt.Fprintln(os.Stderr, "app login:", err)
			return 1
		}
		defer app.Close()
		if err := super.QueryRow(ctx,
			`INSERT INTO hospitals (name, short_code) VALUES ('Synthetic Hospital', 'HSHARED') RETURNING id`).Scan(&seedHospitalID); err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

const serviceKey = "test-only-bff-service-key-0123456789abcdef"

// seedHospitalID is a hospital seeded once, for tests that need one before
// a *testing.T is in scope.
var seedHospitalID uuid.UUID

func seedHospital(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	code := "H" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	if err := super.QueryRow(context.Background(),
		`INSERT INTO hospitals (name, short_code) VALUES ('Synthetic Hospital', $1) RETURNING id`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// chain is what a route looks like: BFF, then (optionally) the token
// middleware setting the principal, then Idempotent (IdempotentAnon for a
// public route), then the handler.
func chain(p *httpx.Principal, h http.Handler) http.Handler {
	inner := httpx.Idempotent(app)(h)
	if p == nil {
		inner = httpx.IdempotentAnon(app)(h)
	}
	withPrincipal := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p != nil {
			r = r.WithContext(httpx.WithPrincipal(r.Context(), *p))
		}
		inner.ServeHTTP(w, r)
	})
	return httpx.BFF([]string{serviceKey})(withPrincipal)
}

func send(h http.Handler, key, ip, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/things", strings.NewReader(body))
	req.Header.Set("X-MHAI-Service", serviceKey)
	if ip != "" {
		req.Header.Set("X-MHAI-Client-IP", ip)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// counting returns a handler that answers 201 with a body naming the call.
func counting(n *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := n.Add(1)
		httpx.JSON(w, http.StatusCreated, map[string]any{"call": c})
	})
}

func rowCount(t *testing.T, scope, key string) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(),
		`SELECT count(*) FROM idempotency_keys WHERE scope = $1 AND key = $2`, scope, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIdempotentReplay(t *testing.T) {
	h := seedHospital(t)
	p := &httpx.Principal{UserID: uuid.NewString(), HospitalID: h}
	var n atomic.Int32
	srv := chain(p, counting(&n))
	key := uuid.NewString()

	first := send(srv, key, "203.0.113.7", `{"a":1}`)
	second := send(srv, key, "203.0.113.8", `{"a":1}`)
	if n.Load() != 1 {
		t.Fatalf("handler ran %d times, want 1", n.Load())
	}
	if first.Code != 201 || second.Code != 201 || first.Body.String() != second.Body.String() {
		t.Fatalf("first %d %q, second %d %q", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("replay not marked")
	}
	var hid uuid.UUID
	if err := super.QueryRow(context.Background(),
		`SELECT hospital_id FROM idempotency_keys WHERE scope = $1 AND key = $2 AND expires_at > now() + interval '71 hours'`,
		p.UserID, key).Scan(&hid); err != nil || hid != h {
		t.Fatalf("stored row: hospital %v, err %v", hid, err)
	}
	// Another user with the same key is a different scope.
	other := chain(&httpx.Principal{UserID: uuid.NewString(), HospitalID: h}, counting(&n))
	if rec := send(other, key, "", `{"a":1}`); rec.Code != 201 || n.Load() != 2 {
		t.Fatalf("other user: %d, calls %d", rec.Code, n.Load())
	}
}

func TestIdempotentConflict(t *testing.T) {
	p := &httpx.Principal{UserID: uuid.NewString(), HospitalID: seedHospital(t)}
	var n atomic.Int32
	srv := chain(p, counting(&n))
	key := uuid.NewString()
	if rec := send(srv, key, "", `{"a":1}`); rec.Code != 201 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := send(srv, key, "", `{"a":2}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"conflict"`) {
		t.Fatalf("different body: %d %s", rec.Code, rec.Body.String())
	}
	if n.Load() != 1 {
		t.Fatalf("handler ran %d times", n.Load())
	}
}

// The same key on another route is a different request: conflict.
func TestIdempotentConflictAcrossPaths(t *testing.T) {
	p := &httpx.Principal{UserID: uuid.NewString(), HospitalID: seedHospital(t)}
	var n atomic.Int32
	srv := chain(p, counting(&n))
	key := uuid.NewString()
	send(srv, key, "", `{}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/other", strings.NewReader(`{}`))
	req.Header.Set("X-MHAI-Service", serviceKey)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || n.Load() != 1 {
		t.Fatalf("status %d, calls %d", rec.Code, n.Load())
	}
}

// A row for the same (scope, key) that RLS hides (another hospital) makes
// the insert hit the primary key: 23505 must come back as 409 conflict.
func TestIdempotentUniqueViolationIsConflict(t *testing.T) {
	a, b := seedHospital(t), seedHospital(t)
	user, key := uuid.NewString(), uuid.NewString()
	if _, err := super.Exec(context.Background(),
		`INSERT INTO idempotency_keys (scope, key, hospital_id, request_hash, status, body) VALUES ($1, $2, $3, 'x', 201, '{}')`,
		user, key, b); err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	rec := send(chain(&httpx.Principal{UserID: user, HospitalID: a}, counting(&n)), key, "", `{}`)
	if rec.Code != http.StatusConflict || n.Load() != 0 {
		t.Fatalf("status %d, calls %d, body %s", rec.Code, n.Load(), rec.Body.String())
	}
}

// Routes that issue tokens opt out: their responses are never stored, and
// a retry runs the handler again.
func TestIdempotentNoStore(t *testing.T) {
	var n atomic.Int32
	tokenRoute := httpx.NoStore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		httpx.JSON(w, http.StatusOK, map[string]string{"access_token": "synthetic-token-value"})
	}))
	srv := chain(nil, tokenRoute)
	key := uuid.NewString()
	for i := 0; i < 2; i++ {
		if rec := send(srv, key, "198.51.100.20", `{"username":"a"}`); rec.Code != 200 {
			t.Fatalf("call %d: %d", i, rec.Code)
		}
	}
	if n.Load() != 2 {
		t.Fatalf("handler ran %d times, want 2", n.Load())
	}
	if c := rowCount(t, "anon:198.51.100.20", key); c != 0 {
		t.Fatalf("%d rows stored for a no-store route", c)
	}
	var leaked int
	_ = super.QueryRow(context.Background(),
		`SELECT count(*) FROM idempotency_keys WHERE position('synthetic-token-value' in convert_from(body, 'UTF8')) > 0`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("token stored in idempotency_keys")
	}
}

// A response marked Cache-Control: no-store is not stored either.
func TestIdempotentCacheControlNoStore(t *testing.T) {
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusOK, map[string]string{"ok": "1"})
	})
	user := uuid.NewString()
	srv := chain(&httpx.Principal{UserID: user}, h)
	key := uuid.NewString()
	send(srv, key, "", `{}`)
	send(srv, key, "", `{}`)
	if n.Load() != 2 || rowCount(t, user, key) != 0 {
		t.Fatalf("calls %d, rows %d", n.Load(), rowCount(t, user, key))
	}
}

// Failed requests are not stored, so the client can retry.
func TestIdempotentErrorsNotStored(t *testing.T) {
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		httpx.Error(w, r, "internal_error")
	})
	user := uuid.NewString()
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)}, h)
	key := uuid.NewString()
	send(srv, key, "", `{}`)
	send(srv, key, "", `{}`)
	if n.Load() != 2 || rowCount(t, user, key) != 0 {
		t.Fatalf("calls %d, rows %d", n.Load(), rowCount(t, user, key))
	}
}

// Before sign-in the scope is the client IP; a platform principal's rows
// carry no hospital.
func TestIdempotentScopes(t *testing.T) {
	var n atomic.Int32
	anon := chain(nil, counting(&n))
	key := uuid.NewString()
	send(anon, key, "198.51.100.1", `{}`)
	send(anon, key, "198.51.100.2", `{}`)
	send(anon, key, "198.51.100.1", `{}`)
	if n.Load() != 2 {
		t.Fatalf("anon calls %d, want 2", n.Load())
	}
	if rowCount(t, "anon:198.51.100.1", key) != 1 || rowCount(t, "anon:198.51.100.2", key) != 1 {
		t.Fatal("anon rows not scoped by IP")
	}

	user := uuid.NewString()
	platform := chain(&httpx.Principal{UserID: user}, counting(&n))
	send(platform, key, "", `{}`)
	send(platform, key, "", `{}`)
	if n.Load() != 3 {
		t.Fatalf("platform calls %d, want 3", n.Load())
	}
	var hid *uuid.UUID
	if err := super.QueryRow(context.Background(),
		`SELECT hospital_id FROM idempotency_keys WHERE scope = $1 AND key = $2`, user, key).Scan(&hid); err != nil || hid != nil {
		t.Fatalf("platform row hospital %v, err %v", hid, err)
	}
}

// Expired keys are forgotten.
func TestIdempotentExpired(t *testing.T) {
	user := uuid.NewString()
	var n atomic.Int32
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)}, counting(&n))
	key := uuid.NewString()
	send(srv, key, "", `{}`)
	if _, err := super.Exec(context.Background(),
		`UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE scope = $1`, user); err != nil {
		t.Fatal(err)
	}
	if rec := send(srv, key, "", `{"changed":true}`); rec.Code != 201 || n.Load() != 2 {
		t.Fatalf("after expiry: %d, calls %d", rec.Code, n.Load())
	}
}

// While the first request runs, a retry gets 409 conflict rather than
// running the handler twice.
func TestIdempotentInFlightConflict(t *testing.T) {
	user := uuid.NewString()
	key := uuid.NewString()
	started, release := make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)}, slow)
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- send(srv, key, "", `{}`) }()
	<-started
	if rec := send(srv, key, "", `{}`); rec.Code != http.StatusConflict {
		t.Errorf("in-flight retry: %d", rec.Code)
	}
	close(release)
	if rec := <-done; rec.Code != http.StatusNoContent {
		t.Fatalf("first: %d", rec.Code)
	}
	if n.Load() != 1 {
		t.Fatalf("calls %d", n.Load())
	}
}

// A handler that panics leaves no reservation behind.
func TestIdempotentPanicReleasesKey(t *testing.T) {
	user := uuid.NewString()
	key := uuid.NewString()
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	func() {
		defer func() { _ = recover() }()
		send(srv, key, "", `{}`)
	}()
	if c := rowCount(t, user, key); c != 0 {
		t.Fatalf("%d rows left after panic", c)
	}
}

// A 2xx body holding a credential (a token, a secret) is never stored,
// even on a route that forgot NoStore and the no-store header. The key is
// kept as a tombstone: the retry gets 409, and the handler never runs twice.
func TestIdempotentNeverStoresSecrets(t *testing.T) {
	user := uuid.NewString()
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		httpx.JSON(w, http.StatusOK, map[string]any{"data": map[string]string{"access_token": "synthetic-token-value"}})
	})
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)}, h)
	key := uuid.NewString()
	if rec := send(srv, key, "", `{}`); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := send(srv, key, "", `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("retry: %d, want 409", rec.Code)
	}
	if n.Load() != 1 {
		t.Fatalf("handler ran %d times", n.Load())
	}
	var leaked int
	_ = super.QueryRow(context.Background(),
		`SELECT count(*) FROM idempotency_keys WHERE body IS NOT NULL AND position('synthetic-token-value' in convert_from(body, 'UTF8')) > 0`).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("token stored in idempotency_keys")
	}
}

// A non-JSON 2xx body can't be stored either: tombstone, retry gets 409.
func TestIdempotentNonJSONTombstone(t *testing.T) {
	user := uuid.NewString()
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("plain text"))
	})
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)}, h)
	key := uuid.NewString()
	send(srv, key, "", `{}`)
	if rec := send(srv, key, "", `{}`); rec.Code != http.StatusConflict || n.Load() != 1 {
		t.Fatalf("retry: %d, calls %d", rec.Code, n.Load())
	}
}

// email and code are not credentials: such bodies are stored and replayed.
func TestIdempotentStoresEmailAndCode(t *testing.T) {
	user := uuid.NewString()
	var n atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		httpx.JSON(w, http.StatusCreated, map[string]any{"email": "grace@havana.example", "code": "I10", "call": n.Load()})
	})
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)}, h)
	key := uuid.NewString()
	first := send(srv, key, "", `{}`)
	second := send(srv, key, "", `{}`)
	if n.Load() != 1 || second.Code != 201 || second.Body.String() != first.Body.String() ||
		second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("calls %d, second %d %q", n.Load(), second.Code, second.Body.String())
	}
}

// When the reply can't be stored, the failure is logged with only a hash
// of the scope and the request id; the key stays reserved.
func TestIdempotentFinishFailureLogged(t *testing.T) {
	user := uuid.NewString()
	key := uuid.NewString()
	var logs bytes.Buffer
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Make the store fail: the reservation vanishes under the handler.
		_, _ = super.Exec(r.Context(), `DELETE FROM idempotency_keys WHERE scope = $1`, user)
		httpx.JSON(w, http.StatusCreated, map[string]string{"ok": "1"})
	})
	inner := httpx.IdempotentLog(app, logx.New(&logs))(h)
	srv := httpx.RequestID(httpx.BFF([]string{serviceKey})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(httpx.WithPrincipal(r.Context(),
			httpx.Principal{UserID: user, HospitalID: seedHospitalID})))
	})))
	rec := send(srv, key, "", `{}`)
	if rec.Code != 201 {
		t.Fatalf("status %d", rec.Code)
	}
	out := logs.String()
	sum := sha256.Sum256([]byte(user))
	if !strings.Contains(out, rec.Header().Get("X-Request-ID")) || !strings.Contains(out, hex.EncodeToString(sum[:8])) {
		t.Fatalf("log lacks request id or scope hash: %s", out)
	}
	if strings.Contains(out, user) || strings.Contains(out, key) {
		t.Fatalf("log holds the scope or key: %s", out)
	}
}

// Identical requests racing on one key: the handler runs once, one gets
// its reply and the rest get 409 conflict (or the replay, if they arrive
// after it is stored).
func TestIdempotentConcurrentReserve(t *testing.T) {
	user := uuid.NewString()
	key := uuid.NewString()
	var n atomic.Int32
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(200 * time.Millisecond)
		httpx.JSON(w, http.StatusCreated, map[string]string{"ok": "1"})
	})
	srv := chain(&httpx.Principal{UserID: user, HospitalID: seedHospital(t)}, slow)
	const racers = 8
	start := make(chan struct{})
	type reply struct {
		code     int
		replayed bool
	}
	replies := make(chan reply, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := send(srv, key, "", `{"same":true}`)
			replies <- reply{rec.Code, rec.Header().Get("Idempotent-Replayed") == "true"}
		}()
	}
	close(start)
	wg.Wait()
	close(replies)
	// A racer scheduled after the first reply was stored gets the replay
	// (201, Idempotent-Replayed) instead of 409: both are correct.
	created, replayed, conflicts := 0, 0, 0
	for r := range replies {
		switch {
		case r.code == http.StatusCreated && r.replayed:
			replayed++
		case r.code == http.StatusCreated:
			created++
		case r.code == http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", r.code)
		}
	}
	if n.Load() != 1 || created != 1 || created+replayed+conflicts != racers {
		t.Fatalf("calls %d, created %d, replayed %d, conflicts %d", n.Load(), created, replayed, conflicts)
	}
}
