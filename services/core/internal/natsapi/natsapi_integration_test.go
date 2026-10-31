//go:build integration

package natsapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/natsapi"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const (
	serviceKey   = "test-only-bff-service-key-0123456789abcdef"
	testMFAKey   = "test-only-mfa-key-0123456789abcdef0123456789"
	testPassword = "violet-harbour-lantern-0731" // synthetic
)

var (
	super, app *pgxpool.Pool
	natsSrv    *testutil.NATS
	gatewayJS  jetstream.JetStream
	buckets    bus.Buckets
	issuer     token.Issuer
	pwHash     string
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	code := func() int {
		pg, stopPG, err := testutil.NewPostgres(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start postgres:", err)
			return 1
		}
		defer stopPG()
		n, stopNATS, err := testutil.NewNATS(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start nats:", err)
			return 1
		}
		defer stopNATS()
		natsSrv = n
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
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
		nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{})
		if err != nil {
			fmt.Fprintln(os.Stderr, "nats connect:", err)
			return 1
		}
		defer nc.Close()
		gatewayJS = js
		if buckets, err = bus.OpenBuckets(ctx, js, 1); err != nil {
			fmt.Fprintln(os.Stderr, "buckets:", err)
			return 1
		}
		if err := bus.EnsureStreams(ctx, js, bus.StreamConfig{Replicas: 1, PipelineMaxAckPending: 8}); err != nil {
			fmt.Fprintln(os.Stderr, "streams:", err)
			return 1
		}
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		issuer = token.Issuer{Key: priv, KeyID: "test-kid"}
		if pwHash, err = password.Hash(testPassword); err != nil {
			fmt.Fprintln(os.Stderr, "hash:", err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// env is core's whole chain (server.Register with the real guard, auth
// service and idempotency stores), served over HTTP (mux.Handler with the
// BFF key) and over NATS (natsapi.Serve on mux.NATSHandler), plus two
// test-only routes registered the same way as every route.
type env struct {
	mux    *authz.Mux
	h      http.Handler // HTTP
	nh     http.Handler // what natsapi serves
	logs   *syncBuffer
	writes *writeCounter
	gate   *gate
	bff    map[string]*nats.Conn // app → the app's BFF connection
}

type writeCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *writeCounter) inc(k string) { c.mu.Lock(); c.n[k]++; c.mu.Unlock() }
func (c *writeCounter) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[k]
}

// gate blocks the test routes while closed, until the request's own
// context ends.
type gate struct {
	mu     sync.Mutex
	closed bool
	open   chan struct{}
}

func newGate() *gate { g := &gate{open: make(chan struct{})}; close(g.open); return g }
func (g *gate) wait(ctx context.Context) bool {
	g.mu.Lock()
	ch := g.open
	g.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}
func (g *gate) shut() { g.mu.Lock(); g.open = make(chan struct{}); g.mu.Unlock() }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{logs: &syncBuffer{}, writes: &writeCounter{n: map[string]int{}}, gate: newGate(), bff: map[string]*nats.Conn{}}
	log := logx.New(e.logs)
	svc := &auth.Service{Pool: app, Buckets: buckets, Issuer: issuer, Keys: issuer.PublicKeys(), Log: log,
		MFAKey: []byte(testMFAKey), Argon: password.NewGate(64, time.Minute)}
	guard := &authz.Guard{Keys: issuer.PublicKeys(), Denylist: buckets.Denylist, Pool: app, Log: log,
		Idem: httpx.IdempotentLog(app, log), IdemAnon: httpx.IdempotentAnonLog(app, log)}
	m := authz.NewMux(guard)
	server.Register(m, server.Deps{Auth: svc, Pool: app})
	// A write that counts how often it really ran, keyed by its
	// Idempotency-Key, behind the same guard and idempotency store.
	authz.Route(m, http.MethodPost, "/_natsapi/write", authz.Rule{AnyRole: []string{authz.RoleClinician}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !e.gate.wait(r.Context()) {
				httpx.Error(w, r, "unavailable")
				return
			}
			e.writes.inc(r.Header.Get("Idempotency-Key"))
			httpx.JSON(w, http.StatusOK, map[string]bool{"ok": true})
		}))
	authz.Route(m, http.MethodGet, "/_natsapi/read", authz.Rule{Public: true},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !e.gate.wait(r.Context()) {
				httpx.Error(w, r, "unavailable")
				return
			}
			httpx.JSON(w, http.StatusOK, map[string]string{"audience": httpx.Audience(r.Context())})
		}))
	e.mux = m
	e.h = m.Handler([]string{serviceKey})
	e.nh = m.NATSHandler()
	for _, a := range []string{natsapi.AppClinical, natsapi.AppPlatform} {
		url := natsSrv.BFFClinicalURL
		if a == natsapi.AppPlatform {
			url = natsSrv.BFFPlatformURL
		}
		e.bff[a] = connect(t, url, bus.BFFInbox(a))
	}
	return e
}

func connect(t *testing.T, url, inbox string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url, nats.CustomInboxPrefix(inbox), nats.MaxReconnects(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// core starts one copy of core's NATS API on its own gateway connection,
// stopped when the test ends.
type coreCopy struct {
	nc      *nats.Conn
	stop    func()
	metrics *natsapi.Metrics
}

func (e *env) core(t *testing.T, h http.Handler, opts ...natsapi.Option) *coreCopy {
	t.Helper()
	if h == nil {
		h = e.nh
	}
	nc := connect(t, natsSrv.GatewayURL, bus.GatewayInboxPrefix)
	c := &coreCopy{nc: nc, metrics: &natsapi.Metrics{}}
	stop, err := natsapi.Serve(context.Background(), nc, h, logx.New(e.logs), append(opts, natsapi.WithMetrics(c.metrics))...)
	if err != nil {
		t.Fatal(err)
	}
	c.stop = stop
	t.Cleanup(stop)
	return c
}

type call struct {
	app, method, path string
	body              string
	bearer            string
	ip                string
	hdr               map[string]string
	timeoutMs         int
	wait              time.Duration
}

type resp struct {
	code int
	hdr  http.Header
	body map[string]any
	raw  string
}

func (c call) defaults() call {
	if c.app == "" {
		c.app = natsapi.AppClinical
	}
	if c.ip == "" {
		c.ip = "198.51.100.7"
	}
	if c.wait == 0 {
		c.wait = 10 * time.Second
	}
	return c
}

// nats sends c the way the BFF does: on api.v1.<app>, from that app's
// user, with the reply on its own inbox.
func (e *env) nats(t *testing.T, c call) (resp, error) {
	t.Helper()
	c = c.defaults()
	m := nats.NewMsg("api.v1." + c.app)
	m.Header.Set(natsapi.HeaderMethod, c.method)
	m.Header.Set(natsapi.HeaderPath, httpx.BasePath+c.path)
	m.Header.Set("Content-Type", "application/json")
	m.Header.Set("X-MHAI-Client-IP", c.ip)
	if c.bearer != "" {
		m.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.timeoutMs > 0 {
		m.Header.Set(natsapi.HeaderTimeout, strconv.Itoa(c.timeoutMs))
	}
	for k, v := range c.hdr {
		m.Header.Set(k, v)
	}
	m.Data = []byte(c.body)
	r, err := e.bff[c.app].RequestMsg(m, c.wait)
	if err != nil {
		return resp{}, err
	}
	out := resp{hdr: http.Header{}, raw: string(r.Data)}
	out.code, _ = strconv.Atoi(r.Header.Get(natsapi.HeaderStatus))
	for k, vs := range r.Header {
		if k == natsapi.HeaderStatus {
			continue
		}
		for _, v := range vs {
			out.hdr.Add(k, v)
		}
	}
	_ = json.Unmarshal(r.Data, &out.body)
	return out, nil
}

func (e *env) mustNATS(t *testing.T, c call) resp {
	t.Helper()
	r, err := e.nats(t, c)
	if err != nil {
		t.Fatalf("%s %s over NATS: %v", c.method, c.path, err)
	}
	return r
}

// http sends c over HTTP with the BFF key, as mhai-web's fetch does today.
func (e *env) http(t *testing.T, c call) resp {
	t.Helper()
	c = c.defaults()
	r := httptest.NewRequest(c.method, httpx.BasePath+c.path, strings.NewReader(c.body))
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-MHAI-Client-IP", c.ip)
	if c.app == natsapi.AppPlatform {
		r.Header.Set("X-MHAI-Audience", "platform")
	}
	if c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for k, v := range c.hdr {
		r.Header.Set(k, v)
	}
	ctx := r.Context()
	if c.timeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.timeoutMs)*time.Millisecond)
		defer cancel()
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r.WithContext(ctx))
	out := resp{code: rec.Code, hdr: rec.Header(), raw: rec.Body.String()}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	return out
}

// --- Seeding (synthetic data only) -----------------------------------------

func uniq() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

func seedHospital(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic General', $1, 'active') RETURNING id`,
		"H"+uniq()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO hospital_settings (hospital_id, idle_timeout_minutes, review_overdue_hours,
		require_mfa_for_clinicians, allow_trust_device) VALUES ($1, 20, 6, false, false)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

type user struct {
	id    uuid.UUID
	email string
	hid   uuid.UUID
	roles []string
}

// seedUser makes an account with testPassword and two-step on. hospital
// is uuid.Nil for a platform account.
func seedUser(t *testing.T, hospital uuid.UUID, active bool, roles ...string) user {
	t.Helper()
	ctx := context.Background()
	email := "synthetic.user." + uniq() + "@havana.example"
	var hid any
	if hospital != uuid.Nil {
		hid = hospital
	}
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, full_name, role_title, is_active)
		VALUES ($1, $2, $3, 'Dr Synthetic', 'Synthetic Person', 'Registrar', $4) RETURNING id`, hid, email, pwHash, active).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := super.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled, backup_code_hashes)
		VALUES ($1, '\x00', true, ARRAY['a','b','c'])`, id); err != nil {
		t.Fatal(err)
	}
	return user{id: id, email: email, hid: hospital, roles: roles}
}

// mint signs in u (a session proven by a second factor) and returns its
// access token.
func mint(t *testing.T, u user) string {
	t.Helper()
	sid := uuid.New()
	amr := []string{"pwd", "otp"}
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, true, $3)`,
		sid, u.id, amr); err != nil {
		t.Fatal(err)
	}
	hid := ""
	if u.hid != uuid.Nil {
		hid = u.hid.String()
	}
	at, _, err := issuer.Access(token.Claims{Sub: u.id.String(), Hid: hid, Roles: u.roles, MFA: true, AMR: amr,
		Sid: sid.String(), Jti: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// --- The reply-subject check (Task 4 ruling: a hard requirement) -------------

// publishRaw sends a request on api.v1.<app> from that app's BFF user with
// a reply subject of the test's choosing, as a stolen BFF credential could.
func publishRaw(t *testing.T, nc *nats.Conn, app, reply string) {
	t.Helper()
	m := nats.NewMsg("api.v1." + app)
	m.Reply = reply
	m.Header.Set(natsapi.HeaderMethod, http.MethodGet)
	m.Header.Set(natsapi.HeaderPath, "/api/v1/_natsapi/read")
	if err := nc.PublishMsg(m); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
}

// TestForeignReplySubjectDropped: a reply subject that isn't the sending
// app's own inbox gets no reply at all and runs no handler. Each case is
// one that, answered, would be a publish with core's permissions: core's
// answer in the other app's inbox, a job on JOBS, a denylist write, a
// JetStream API call, a wildcard.
func TestForeignReplySubjectDropped(t *testing.T) {
	e := newEnv(t)
	var ran atomic.Int64
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran.Add(1); e.nh.ServeHTTP(w, r) })
	c := e.core(t, counting)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Watchers: the platform app's inbox, the clinical app's whole inbox
	// (for the wildcard cases), and core's own inbox.
	platformInbox, err := e.bff[natsapi.AppPlatform].SubscribeSync(bus.BFFInbox("platform") + ".>")
	if err != nil {
		t.Fatal(err)
	}
	clinicalInbox, err := e.bff[natsapi.AppClinical].SubscribeSync(bus.BFFInbox("clinical") + ".>")
	if err != nil {
		t.Fatal(err)
	}
	gatewayInbox, err := c.nc.SubscribeSync(bus.GatewayInboxPrefix + ".>")
	if err != nil {
		t.Fatal(err)
	}
	for _, nc := range []*nats.Conn{e.bff[natsapi.AppPlatform], e.bff[natsapi.AppClinical], c.nc} {
		_ = nc.Flush()
	}

	job := "jobs.v1." + uuid.NewString() + ".run_query"
	kvKey := "probe" + uniq()
	cases := []struct{ app, reply string }{
		{natsapi.AppClinical, bus.BFFInbox("platform") + ".x"},
		{natsapi.AppPlatform, bus.BFFInbox("clinical") + ".x"},
		{natsapi.AppClinical, job},
		{natsapi.AppClinical, "$KV." + bus.DenylistBucket + "." + kvKey},
		{natsapi.AppClinical, "$JS.API.STREAM.INFO." + bus.JobsStream},
		{natsapi.AppClinical, bus.BFFInbox("clinical") + ".*"},
		{natsapi.AppClinical, bus.BFFInbox("clinical") + ".>"},
		{natsapi.AppClinical, bus.BFFInbox("clinical") + ".a*b"},
		{natsapi.AppClinical, bus.BFFInbox("clinical") + "..x"},
		{natsapi.AppClinical, bus.GatewayInboxPrefix + ".x"},
	}
	for _, cs := range cases {
		publishRaw(t, e.bff[cs.app], cs.app, cs.reply)
	}
	time.Sleep(1500 * time.Millisecond)

	for name, sub := range map[string]*nats.Subscription{"platform inbox": platformInbox, "clinical inbox": clinicalInbox, "core inbox": gatewayInbox} {
		if m, err := sub.NextMsg(100 * time.Millisecond); err == nil {
			t.Errorf("%s got a reply on %q", name, m.Subject)
		}
	}
	js, err := jetstream.New(c.nc)
	if err != nil {
		t.Fatal(err)
	}
	st, err := js.Stream(ctx, bus.JobsStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := st.Info(ctx, jetstream.WithSubjectFilter(job))
	if err != nil {
		t.Fatal(err)
	}
	if n := info.State.Subjects[job]; n != 0 {
		t.Errorf("JOBS holds %d messages on %s", n, job)
	}
	if _, err := buckets.Denylist.Get(ctx, kvKey); !bus.IsNotFound(err) {
		t.Errorf("denylist key written: %v", err)
	}
	if ran.Load() != 0 {
		t.Errorf("the handler ran %d times", ran.Load())
	}
	if got := c.metrics.RefusedReply.Load(); got != int64(len(cases)) {
		t.Errorf("refusal metric %d, want %d", got, len(cases))
	}
	logs := e.logs.String()
	if strings.Count(logs, "natsapi: reply subject refused") != len(cases) {
		t.Errorf("refusals not logged: %s", logs)
	}
	for _, leak := range []string{"jobs.v1", "$KV", "$JS", kvKey} {
		if strings.Contains(logs, leak) {
			t.Errorf("log carries the reply subject (%q): %s", leak, logs)
		}
	}

	// The app's own inbox works.
	r := e.mustNATS(t, call{method: "GET", path: "/_natsapi/read"})
	if r.code != 200 || r.body["audience"] != "clinical" || ran.Load() != 1 {
		t.Fatalf("valid reply: %d %s (ran %d)", r.code, r.raw, ran.Load())
	}
	r = e.mustNATS(t, call{app: natsapi.AppPlatform, method: "GET", path: "/_natsapi/read"})
	if r.code != 200 || r.body["audience"] != "platform" {
		t.Fatalf("valid platform reply: %d %s", r.code, r.raw)
	}
}

// TestAPIConcurrencyBounded: past the pool, an immediate 503 unavailable,
// and the pool frees as handlers finish.
func TestAPIConcurrencyBounded(t *testing.T) {
	e := newEnv(t)
	c := e.core(t, nil, natsapi.WithConcurrency(2))
	e.gate.shut()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.nats(t, call{method: "GET", path: "/_natsapi/read", timeoutMs: 3000, wait: 5 * time.Second})
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.metrics.InFlight.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the two calls never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	r := e.mustNATS(t, call{method: "GET", path: "/_natsapi/read", wait: 2 * time.Second})
	if r.code != 503 || r.body["code"] != "unavailable" || time.Since(start) > time.Second {
		t.Fatalf("overflow: %d %s after %v", r.code, r.raw, time.Since(start))
	}
	if r.hdr.Get("X-Request-ID") == "" || r.body["request_id"] != r.hdr.Get("X-Request-ID") {
		t.Fatalf("overflow reply has no request id: %v %s", r.hdr, r.raw)
	}
	if c.metrics.Overflow.Load() != 1 {
		t.Fatalf("overflow metric %d", c.metrics.Overflow.Load())
	}
	wg.Wait() // both held calls end at their 3 s deadline
	deadline = time.Now().Add(5 * time.Second)
	for c.metrics.InFlight.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("pool never freed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestTwoCoreCopiesShareAPI: two copies of core in queue group core share
// api.v1; each call is answered once, by one of them. Then one copy dies
// holding calls: they fail at the BFF's timeout, and retried (writes with
// the same Idempotency-Key) are answered by the other copy. No call is lost
// and no write runs twice.
func TestTwoCoreCopiesShareAPI(t *testing.T) {
	e := newEnv(t)
	gateB := newGate()
	viaGate := func(g *gate) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !g.wait(r.Context()) {
				return // a crashed copy: nothing runs, nothing is answered
			}
			e.nh.ServeHTTP(w, r)
		})
	}
	a := e.core(t, e.nh)
	b := e.core(t, viaGate(gateB))

	const n = 200
	var wg sync.WaitGroup
	var failed atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.nats(t, call{method: "GET", path: "/_natsapi/read"})
			if err != nil || r.code != 200 {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if failed.Load() != 0 {
		t.Fatalf("%d of %d calls failed", failed.Load(), n)
	}
	sa, sb := a.metrics.Served.Load(), b.metrics.Served.Load()
	if sa+sb != n || sa == 0 || sb == 0 {
		t.Fatalf("served a=%d b=%d, want %d in all and some each", sa, sb, n)
	}

	// Copy b hangs (then dies) holding whatever reaches it.
	hid := seedHospital(t)
	tok := mint(t, seedUser(t, hid, true, authz.RoleClinician))
	gateB.shut()
	const writes = 40
	keys := make([]string, writes)
	timedOut := make([]bool, writes)
	for i := range keys {
		keys[i] = "key-" + uuid.NewString()
	}
	write := func(i int) (resp, error) {
		return e.nats(t, call{method: "POST", path: "/_natsapi/write", body: `{"n":1}`, bearer: tok,
			hdr: map[string]string{"Idempotency-Key": keys[i]}, timeoutMs: 2000, wait: 1500 * time.Millisecond})
	}
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := write(i)
			if err != nil {
				timedOut[i] = true
				return
			}
			if r.code != 200 {
				t.Errorf("write %d: %d %s", i, r.code, r.raw)
			}
		}(i)
	}
	time.Sleep(500 * time.Millisecond)
	b.nc.Close() // b dies with its calls
	wg.Wait()
	held := 0
	for i := range keys {
		if !timedOut[i] {
			continue
		}
		held++
		r, err := write(i) // the BFF's retry, same key
		if err != nil || r.code != 200 {
			t.Fatalf("retry %d: %v %d %s", i, err, r.code, r.raw)
		}
	}
	if held == 0 {
		t.Fatal("copy b held no call: the kill wasn't exercised")
	}
	for i, k := range keys {
		if got := e.writes.get(k); got != 1 {
			t.Errorf("write %d ran %d times", i, got)
		}
	}
}

// Over NATS a failed sign-in writes its audit row under core's request id,
// never the BFF's X-Request-ID or Mhai-Request-Id, and leaves no email,
// password or token in the log (R4).
func TestNoCredentialInLogs(t *testing.T) {
	e := newEnv(t)
	e.core(t, nil)
	hid := seedHospital(t)
	u := seedUser(t, hid, true, authz.RoleClinician)
	forged := "forged-" + uniq()
	bearer := "synthetic.bearer." + uniq()
	for _, pw := range []string{"wrong-password-" + uniq(), testPassword + "x"} {
		body, _ := json.Marshal(map[string]string{"username": u.email, "password": pw})
		r := e.mustNATS(t, call{method: "POST", path: "/auth/login", body: string(body), ip: "203.0.113.50",
			hdr: map[string]string{"X-Request-ID": forged, "Mhai-Request-Id": forged, "Authorization": "Bearer " + bearer}})
		if r.code != 401 || r.body["code"] != "unauthenticated" {
			t.Fatalf("failed sign-in: %d %s", r.code, r.raw)
		}
		rid := r.hdr.Get("X-Request-ID")
		if rid == "" || rid == forged || r.body["request_id"] != rid {
			t.Fatalf("request id %q (body %v)", rid, r.body["request_id"])
		}
		var n int
		if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE request_id = $1 AND action = 'auth.login.failed'`,
			rid).Scan(&n); err != nil || n != 1 {
			t.Fatalf("audit rows under core's id: %d %v", n, err)
		}
		if strings.Contains(e.logs.String(), pw) {
			t.Fatal("password in the log")
		}
	}
	var forgedRows int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE request_id = $1`, forged).Scan(&forgedRows)
	if forgedRows != 0 {
		t.Fatalf("%d audit rows carry the BFF's request id", forgedRows)
	}
	logs := e.logs.String()
	for _, leak := range []string{u.email, testPassword, bearer, forged} {
		if strings.Contains(logs, leak) {
			t.Fatalf("log carries %q: %s", leak, logs)
		}
	}
}

// The docs routes are never served over NATS, the probes are HTTP only,
// and the BFF key is not what opens the NATS path.
func TestDocsAndProbesNotOverNATS(t *testing.T) {
	e := newEnv(t)
	e.core(t, nil)
	for _, p := range []string{"/api/docs", "/api/openapi.yaml", "/api/docs/intelligence", "/api/v1/healthz", "/api/v1/readyz"} {
		m := nats.NewMsg("api.v1.clinical")
		m.Header.Set(natsapi.HeaderMethod, "GET")
		m.Header.Set(natsapi.HeaderPath, p)
		r, err := e.bff[natsapi.AppClinical].RequestMsg(m, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.Unmarshal(r.Data, &body)
		if r.Header.Get(natsapi.HeaderStatus) != "404" || body["code"] != "not_found" || r.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s over NATS: %s %s", p, r.Header.Get(natsapi.HeaderStatus), r.Data)
		}
	}
	// The HTTP path still needs the key.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/_natsapi/read", nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("HTTP without the BFF key: %d", rec.Code)
	}
	// And the NATS handler can't be reached over HTTP with it either.
	r = httptest.NewRequest(http.MethodGet, "/api/v1/_natsapi/read", nil)
	r.Header.Set("X-MHAI-Service", serviceKey)
	rec = httptest.NewRecorder()
	e.nh.ServeHTTP(rec, r)
	if rec.Code != 401 {
		t.Fatalf("NATS handler over HTTP: %d", rec.Code)
	}
}

// TestDeadlineEndsHandler (live): a route that blocks is cancelled at the
// BFF's Mhai-Timeout-Ms, and the BFF gets 503 unavailable then.
func TestDeadlineEndsHandlerLive(t *testing.T) {
	e := newEnv(t)
	e.core(t, nil)
	e.gate.shut()
	start := time.Now()
	r := e.mustNATS(t, call{method: "GET", path: "/_natsapi/read", timeoutMs: 300, wait: 5 * time.Second})
	took := time.Since(start)
	if r.code != 503 || r.body["code"] != "unavailable" || took < 250*time.Millisecond || took > 3*time.Second {
		t.Fatalf("%d %s after %v", r.code, r.raw, took)
	}
}
