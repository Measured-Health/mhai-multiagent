//go:build integration

package platform_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// Plan 4 Task 11: GET /platform/health and the sampler (spec §6.1;
// contract §14.8.2; mhai-web's ops-contract.ts HealthOut; abuse case C30).
// Every name and id here is synthetic.

// fakePipeline answers pipeline.health as the pipeline user, in its queue
// group, and counts the calls. It checks each call's token as a worker
// would and keeps the claims.
type fakePipeline struct {
	calls    atomic.Int32
	delay    atomic.Int64 // nanoseconds before answering
	notReady atomic.Bool  // database_ready false
	noModels atomic.Bool  // models_ready false
	mu       sync.Mutex
	claims   []reqtoken.Claims
	bad      []error
}

func startPipeline(t *testing.T) *fakePipeline {
	t.Helper()
	nc, _, err := bus.Connect(context.Background(), natsSrv.PipelineURL, "", bus.WorkerInbox("pipeline"), bus.TLS{})
	if err != nil {
		t.Fatalf("fake pipeline: %v", err)
	}
	f := &fakePipeline{}
	keys := map[string]ed25519.PublicKey{reqSigner.KeyID: reqSigner.Key.Public().(ed25519.PublicKey)}
	sub, err := nc.QueueSubscribe("inference.v1.pipeline.health", "pipeline", func(m *nats.Msg) {
		f.calls.Add(1)
		c, verr := reqtoken.VerifyAt(m.Header.Get("Mhai-Request-Token"), keys,
			reqtoken.Expect{Aud: "pipeline", Subject: m.Subject, Kind: reqtoken.KindReq}, m.Data, time.Now())
		f.mu.Lock()
		f.claims = append(f.claims, c)
		if verr != nil {
			f.bad = append(f.bad, verr)
		}
		f.mu.Unlock()
		time.Sleep(time.Duration(f.delay.Load()))
		body, _ := proto.Marshal(&intelpb.HealthResponse{ModelsReady: !f.noModels.Load(), DatabaseReady: !f.notReady.Load()})
		out := nats.NewMsg(m.Reply)
		out.Header.Set("Mhai-Status", "ok")
		out.Header.Set("Mhai-Schema", "mhai.intelligence.v1.HealthResponse")
		out.Header.Set("Mhai-Request-Id", m.Header.Get("Mhai-Request-Id"))
		out.Data = body
		_ = m.RespondMsg(out)
	})
	if err != nil {
		nc.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sub.Unsubscribe()
		_ = nc.Flush()
		nc.Close()
	})
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakePipeline) seen() ([]reqtoken.Claims, []error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.claims), slices.Clone(f.bad)
}

// statementCounter counts every statement the app pool starts, whether it
// succeeds or not (pgx.QueryTracer: Exec, Query, QueryRow, and the BEGIN
// and COMMIT of every transaction).
type statementCounter struct{ n atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

var appStatements = &statementCounter{}

// failingPut is the health bucket with every Put refused.
type failingPut struct {
	jetstream.KeyValue
	puts atomic.Int32
}

func (f *failingPut) Put(context.Context, string, []byte) (uint64, error) {
	f.puts.Add(1)
	return 0, errors.New("synthetic: put refused")
}

func sampler(now func() time.Time) platform.Sampler {
	return platform.Sampler{Pool: app, Intel: intelClient, KV: healthKV,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: now}
}

// clearHealth removes the stored reading, so a test starts with none.
func clearHealth(t *testing.T) {
	t.Helper()
	if err := healthKV.Delete(context.Background(), platform.HealthKey); err != nil && !bus.IsNotFound(err) {
		t.Fatal(err)
	}
}

func sample(t *testing.T, now func() time.Time) platform.HealthOut {
	t.Helper()
	h, err := sampler(now).Sample(context.Background())
	if err != nil {
		t.Fatalf("sample: %v", err)
	}
	return h
}

func putRaw(t *testing.T, b []byte) {
	t.Helper()
	if _, err := healthKV.Put(context.Background(), platform.HealthKey, b); err != nil {
		t.Fatal(err)
	}
}

// seedQuestion adds a question of hospital hid as the superuser: status,
// error code (""), created and finished ago before now, taking ms, with
// step timings when given.
func seedQuestion(t *testing.T, hid, by uuid.UUID, status, code string, ago time.Duration, ms int, timings string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	var errCode, done any
	if code != "" {
		errCode = code
	}
	end := time.Now().Add(-ago)
	if status == "completed" || status == "failed" {
		done = end
	}
	if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status, error, created_by, created_at, completed_at)
		VALUES ($1, $2, 'SYNTHETIC question about a synthetic patient', 'routine', $3, $4, $5, $6, $7)`,
		id, hid, status, errCode, by, end.Add(-time.Duration(ms)*time.Millisecond), done); err != nil {
		t.Fatalf("seed question: %v", err)
	}
	if timings != "" {
		if _, err := super.Exec(ctx, `INSERT INTO clinical_decisions (id, query_id, hospital_id, timings_ms) VALUES ($1, $2, $3, $4::jsonb)`,
			uuid.New(), id, hid, timings); err != nil {
			t.Fatalf("seed decision: %v", err)
		}
	}
	return id
}

func partStatus(h platform.HealthOut, key string) string {
	for _, p := range h.Parts {
		if p.Key == key {
			return p.Status
		}
	}
	return ""
}

func requireKeys(t *testing.T, what string, v any, want ...string) map[string]any {
	t.Helper()
	m, isMap := v.(map[string]any)
	if !isMap {
		t.Fatalf("%s: not an object: %v", what, v)
	}
	sort.Strings(want)
	if got := keysOf(m); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: keys %v, want %v", what, got, want)
	}
	return m
}

func list(t *testing.T, what string, v any) []any {
	t.Helper()
	l, isList := v.([]any)
	if !isList {
		t.Fatalf("%s: not an array: %v", what, v)
	}
	return l
}

func wholeNonNeg(t *testing.T, what string, v any) float64 {
	t.Helper()
	n, isNum := v.(float64)
	if !isNum || n < 0 || n != float64(int64(n)) {
		t.Fatalf("%s: %v is not a whole non-negative number", what, v)
	}
	return n
}

// TestHealthShape: the reading has exactly ops-contract.ts's HealthOut
// keys and enum values, without documents and clean_up (decision 7) and
// with triage off; both platform roles read it; hospital roles and
// sessions without two-step don't.
func TestHealthShape(t *testing.T) {
	clearHealth(t)
	f := startPipeline(t)
	h := seedHospital(t, "active")
	u := seedUser(t, h, "", authz.RoleClinician)
	seedQuestion(t, h, u.id, "completed", "", 5*time.Minute, 1200, `{"privacy": 50, "guidelines": 300, "answer": 700, "evidence": 150}`)
	seedQuestion(t, h, u.id, "failed", "deadline_exceeded", 2*time.Minute, 0, "")
	seedQuestion(t, h, u.id, "queued", "", 0, 0, "")
	before := time.Now().UTC().Add(-time.Second)
	sample(t, nil)

	tok := mint(t, auditor(t))
	r := get(t, "/platform/health", tok)
	ok(t, "auditor health", r)
	body := requireKeys(t, "reading", r.body,
		"updated_at", "overall", "parts", "queue", "failed_jobs", "timing", "errors", "versions", "triage")
	at, err := time.Parse(time.RFC3339Nano, body["updated_at"].(string))
	if err != nil || at.Before(before) || at.After(time.Now().Add(time.Second)) {
		t.Errorf("updated_at %v (%v): not the sampling time", body["updated_at"], err)
	}
	statuses := []string{"ok", "slow", "problem"}
	if !slices.Contains(statuses, body["overall"].(string)) {
		t.Errorf("overall %v", body["overall"])
	}
	var keys []string
	for i, p := range list(t, "parts", body["parts"]) {
		pm := requireKeys(t, fmt.Sprint("part ", i), p, "key", "status", "since")
		keys = append(keys, pm["key"].(string))
		if !slices.Contains(statuses, pm["status"].(string)) {
			t.Errorf("part %v status %v", pm["key"], pm["status"])
		}
		if s, err := time.Parse(time.RFC3339Nano, pm["since"].(string)); err != nil || s.After(at) {
			t.Errorf("part %v since %v", pm["key"], pm["since"])
		}
	}
	// ops-contract.ts PART_KEYS, less documents and clean_up.
	if strings.Join(keys, ",") != "app,workers,evidence,guidelines" {
		t.Errorf("parts %v", keys)
	}
	q := requireKeys(t, "queue", body["queue"], "waiting", "oldest_wait_s")
	if wholeNonNeg(t, "queue.waiting", q["waiting"]) < 1 || q["oldest_wait_s"] == nil {
		t.Errorf("queue %v: the queued question isn't counted", q)
	}
	wholeNonNeg(t, "queue.oldest_wait_s", q["oldest_wait_s"])
	fj := requireKeys(t, "failed_jobs", body["failed_jobs"], "now", "last_24h")
	if wholeNonNeg(t, "failed_jobs.now", fj["now"]) < 1 || wholeNonNeg(t, "failed_jobs.last_24h", fj["last_24h"]) < 1 {
		t.Errorf("failed_jobs %v: the failed question isn't counted", fj)
	}
	tm := requireKeys(t, "timing", body["timing"], "last_hour", "last_24h")
	for _, w := range []string{"last_hour", "last_24h"} {
		b := requireKeys(t, w, tm[w], "overall", "steps")
		o := requireKeys(t, w+".overall", b["overall"], "p50_ms", "p95_ms")
		if wholeNonNeg(t, w+" p50", o["p50_ms"]) == 0 {
			t.Errorf("%s overall %v: the completed question isn't counted", w, o)
		}
		wholeNonNeg(t, w+" p95", o["p95_ms"])
		var steps []string
		for _, s := range list(t, w+".steps", b["steps"]) {
			sm := requireKeys(t, w+" step", s, "step", "p50_ms", "p95_ms")
			steps = append(steps, sm["step"].(string))
			wholeNonNeg(t, "p50", sm["p50_ms"])
			wholeNonNeg(t, "p95", sm["p95_ms"])
		}
		// ops-contract.ts STEP_KEYS, in order.
		if strings.Join(steps, ",") != "privacy,guidelines,answer,evidence" {
			t.Errorf("%s steps %v", w, steps)
		}
	}
	var kinds []string
	for _, e := range list(t, "errors", body["errors"]) {
		em := requireKeys(t, "error", e, "kind", "last_hour", "last_24h")
		kinds = append(kinds, em["kind"].(string))
		wholeNonNeg(t, "last_hour", em["last_hour"])
		if em["kind"] == "timeout" && wholeNonNeg(t, "timeout last_24h", em["last_24h"]) < 1 {
			t.Errorf("deadline_exceeded not counted as timeout: %v", em)
		}
	}
	// ops-contract.ts ERROR_KINDS, in order.
	if strings.Join(kinds, ",") != "timeout,unreachable,unreadable,invalid,other" {
		t.Errorf("errors %v", kinds)
	}
	if v := list(t, "versions", body["versions"]); len(v) != 0 {
		t.Errorf("versions %v: nothing reports model versions yet", v)
	}
	if tr := requireKeys(t, "triage", body["triage"], "enabled"); tr["enabled"] != false {
		t.Errorf("triage %v", tr)
	}
	for _, gone := range []string{`"documents"`, `"clean_up"`} {
		if strings.Contains(r.raw, gone) {
			t.Errorf("the reading has %s (decision 7)", gone)
		}
	}
	// The worker answered: workers, evidence and app are ok.
	for _, p := range list(t, "parts", body["parts"]) {
		pm := p.(map[string]any)
		if pm["key"] != "guidelines" && pm["status"] != "ok" {
			t.Errorf("part %v is %v with a ready worker", pm["key"], pm["status"])
		}
	}
	// The probe is svc:core with no roles, for no real hospital.
	claims, bad := f.seen()
	if len(bad) != 0 || len(claims) != 1 || claims[0].Sub != reqtoken.SvcCore || len(claims[0].Roles) != 0 {
		t.Errorf("pipeline.health calls: %+v, errors %v", claims, bad)
	}
	var probeIsHospital bool
	if err := super.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM hospitals WHERE id::text = $1)`,
		claims[0].Hid).Scan(&probeIsHospital); err != nil || probeIsHospital {
		t.Errorf("the probe names hospital %s (%v)", claims[0].Hid, err)
	}

	// Both platform roles read; the rest don't.
	ok(t, "platform admin health", get(t, "/platform/health", mint(t, seedUser(t, uuid.Nil, "", authz.RolePlatformAdmin))))
	if cc := doRaw(t, "/platform/health", tok).Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	problem(t, "a query parameter", get(t, "/platform/health?x=1", tok), 422, "validation_failed")
	for _, roles := range [][]string{{authz.RoleClinician}, {authz.RoleHospitalAdmin}, {authz.RoleAnalyst}} {
		ht := mint(t, seedUser(t, h, "", roles...))
		problem(t, fmt.Sprint(roles, " platform"), do(t, "GET", "/platform/health", ht, true), 403, "not_platform")
		problem(t, fmt.Sprint(roles, " clinical"), do(t, "GET", "/platform/health", ht, false), 403, "not_platform")
	}
	problem(t, "auditor on the clinical audience", do(t, "GET", "/platform/health", tok, false), 403, "not_platform")
	problem(t, "anonymous", get(t, "/platform/health", ""), 401, "unauthenticated")
	for _, role := range []string{authz.RolePlatformAdmin, authz.RoleAuditor} {
		nt := signIn(t, seedUser(t, uuid.Nil, "", role), false).tok
		problem(t, role+" without two-step", get(t, "/platform/health", nt), 403, "mfa_required")
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if r := do(t, m, "/platform/health", tok, true); r.code < 400 {
			t.Errorf("%s /platform/health: %d", m, r.code)
		}
	}
}

// doRaw is a platform GET that returns the recorder, for headers.
func doRaw(t *testing.T, path, tok string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, httpx.BasePath+path, nil)
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("X-MHAI-Client-IP", "198.51.100.9")
	r.Header.Set("X-MHAI-Audience", "platform")
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// TestHealthStaleIs503: no reading, one older than two minutes, one dated
// ahead, and one not of the contract's shape are 503 unavailable, never a
// reading of zeros.
func TestHealthStaleIs503(t *testing.T) {
	startPipeline(t)
	tok := mint(t, auditor(t))
	clearHealth(t)
	problem(t, "no reading", get(t, "/platform/health", tok), 503, "unavailable")

	sample(t, func() time.Time { return time.Now().Add(-90 * time.Second) })
	ok(t, "a reading 90 s old", get(t, "/platform/health", tok))
	sample(t, func() time.Time { return time.Now().Add(-121 * time.Second) })
	problem(t, "a reading over 2 minutes old", get(t, "/platform/health", tok), 503, "unavailable")
	sample(t, func() time.Time { return time.Now().Add(5 * time.Minute) })
	problem(t, "a reading dated ahead", get(t, "/platform/health", tok), 503, "unavailable")

	good := sample(t, nil)
	ok(t, "a fresh reading", get(t, "/platform/health", tok))
	raw, err := json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	tamper := func(name string, change func(map[string]any)) {
		t.Helper()
		c := map[string]any{}
		b, _ := json.Marshal(m)
		_ = json.Unmarshal(b, &c)
		change(c)
		b, _ = json.Marshal(c)
		putRaw(t, b)
		problem(t, name, get(t, "/platform/health", tok), 503, "unavailable")
	}
	tamper("an unknown field (clean_up)", func(c map[string]any) {
		c["clean_up"] = map[string]any{"last_run_at": nil, "ok": nil, "paused": false, "paused_by": nil, "paused_at": nil}
	})
	tamper("a documents part", func(c map[string]any) {
		c["parts"] = append(c["parts"].([]any), map[string]any{"key": "documents", "status": "ok", "since": c["updated_at"]})
	})
	tamper("an unknown status", func(c map[string]any) { c["overall"] = "fine" })
	tamper("an unknown kind", func(c map[string]any) {
		c["errors"].([]any)[0].(map[string]any)["kind"] = "boom"
	})
	tamper("a negative count", func(c map[string]any) { c["failed_jobs"].(map[string]any)["now"] = -1 })
	tamper("triage on", func(c map[string]any) { c["triage"] = map[string]any{"enabled": true} })
	tamper("a hospital", func(c map[string]any) { c["hospital"] = map[string]any{"id": uuid.NewString()} })
	tamper("an unsafe version label", func(c map[string]any) {
		c["versions"] = []any{map[string]any{"part": "evidence", "label": "<script>"}}
	})
	tamper("no updated_at", func(c map[string]any) { delete(c, "updated_at") })
	tamper("overall better than a part", func(c map[string]any) {
		c["parts"].([]any)[1].(map[string]any)["status"] = "problem"
		c["overall"] = "ok"
	})
	putRaw(t, []byte(`not json`))
	problem(t, "not JSON", get(t, "/platform/health", tok), 503, "unavailable")
	putRaw(t, append(raw, []byte(` {}`)...))
	problem(t, "trailing data", get(t, "/platform/health", tok), 503, "unavailable")
}

// statementCalls is how many statements every role but the superuser
// running this query has executed (pg_stat_statements): the app login and
// every role it SETs (mhai_auth, mhai_platform, mhai_jobs, ...).
func statementCalls(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := super.QueryRow(context.Background(), `
		SELECT COALESCE(sum(s.calls), 0)::bigint FROM pg_stat_statements s
		JOIN pg_roles r ON r.oid = s.userid WHERE r.rolname <> current_user`).Scan(&n); err != nil {
		t.Fatalf("pg_stat_statements: %v", err)
	}
	return n
}

// TestHealthPollsDontQuery (C30): 100 parallel GETs run no database
// statement and make no pipeline.health call beyond the sampler's: the
// route reads the KV bucket only. Statements are counted twice: by a pgx
// tracer on the app pool (every statement started, failed ones too) and
// by pg_stat_statements on the server.
//
// C30 excludes the principal middleware's per-session state read
// (authz.Guard, cached up to 30 s): it is part of authenticating any
// request, not of the health route, so one warm-up GET comes first.
func TestHealthPollsDontQuery(t *testing.T) {
	clearHealth(t)
	f := startPipeline(t)
	tok := mint(t, auditor(t))

	before, beforeSrv := appStatements.n.Load(), statementCalls(t)
	sample(t, nil)
	if appStatements.n.Load() <= before || statementCalls(t) <= beforeSrv {
		t.Fatal("the sampler's statements weren't counted: the measurement proves nothing")
	}
	if f.calls.Load() != 1 {
		t.Fatalf("%d pipeline.health calls for one sample", f.calls.Load())
	}
	ok(t, "warm-up", get(t, "/platform/health", tok))

	start, startSrv := appStatements.n.Load(), statementCalls(t)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := doRaw(t, "/platform/health", tok); w.Code != http.StatusOK {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := failed.Load(); n != 0 {
		t.Fatalf("%d of 100 polls failed", n)
	}
	if d := appStatements.n.Load() - start; d != 0 {
		t.Errorf("100 polls started %d statements on the app pool, want 0", d)
	}
	if d := statementCalls(t) - startSrv; d != 0 {
		t.Errorf("100 polls ran %d statements on the server, want 0", d)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("100 polls made %d pipeline.health calls beyond the sampler's", n-1)
	}
}

// TestSamplerOneLeader: of two samplers on the same database, one samples
// (the advisory lock); when it stops, the other takes over within a tick.
func TestSamplerOneLeader(t *testing.T) {
	clearHealth(t)
	startPipeline(t)
	var counts [2]atomic.Int32
	ctxs := [2]context.Context{}
	cancels := [2]context.CancelFunc{}
	done := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	for i := range 2 {
		ctxs[i], cancels[i] = context.WithCancel(context.Background())
		s := sampler(nil)
		s.Sampled = func(platform.HealthOut) { counts[i].Add(1) }
		go func() {
			defer close(done[i])
			if err := s.Run(ctxs[i], 100*time.Millisecond); err != nil {
				t.Errorf("sampler %d: %v", i, err)
			}
		}()
	}
	t.Cleanup(func() {
		for i := range 2 {
			cancels[i]()
			<-done[i]
		}
	})
	time.Sleep(1500 * time.Millisecond)
	a, b := counts[0].Load(), counts[1].Load()
	if (a == 0) == (b == 0) {
		t.Fatalf("samples per sampler: %d and %d, want exactly one sampling", a, b)
	}
	leader, other := 0, 1
	if a == 0 {
		leader, other = 1, 0
	}
	if counts[leader].Load() < 5 {
		t.Errorf("the leader sampled %d times in 1.5 s at 100 ms", counts[leader].Load())
	}
	cancels[leader]()
	<-done[leader]
	deadline := time.Now().Add(3 * time.Second)
	for counts[other].Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if counts[other].Load() == 0 {
		t.Fatal("the other sampler didn't take over after the leader stopped")
	}
	// The reading is fresh and served.
	ok(t, "health after the takeover", get(t, "/platform/health", mint(t, auditor(t))))
}

// TestHealthWorkersProblemWhenNoResponders: no worker running makes
// workers and evidence problem (and overall); a slow answer is slow; a
// worker whose database or models aren't ready is problem. A part keeps
// its since while its status holds.
func TestHealthWorkersProblemWhenNoResponders(t *testing.T) {
	clearHealth(t)
	tok := mint(t, auditor(t))
	h := sample(t, nil)
	if partStatus(h, "workers") != "problem" || partStatus(h, "evidence") != "problem" || h.Overall != "problem" ||
		partStatus(h, "app") != "ok" {
		t.Fatalf("no worker: %+v", h.Parts)
	}
	r := get(t, "/platform/health", tok)
	ok(t, "health with no worker", r)
	if r.body["overall"] != "problem" {
		t.Errorf("served overall %v", r.body["overall"])
	}
	since := h.Parts[1].Since
	time.Sleep(20 * time.Millisecond)
	if h2 := sample(t, nil); h2.Parts[1].Since != since || h2.UpdatedAt == h.UpdatedAt {
		t.Errorf("workers since moved while it stayed problem: %s → %s", since, h2.Parts[1].Since)
	}

	f := startPipeline(t)
	h = sample(t, nil)
	if partStatus(h, "workers") != "ok" || partStatus(h, "evidence") != "ok" || h.Parts[1].Since == since {
		t.Errorf("a ready worker: %+v", h.Parts)
	}
	f.delay.Store(int64(1200 * time.Millisecond))
	if h = sample(t, nil); partStatus(h, "workers") != "slow" || h.Overall == "ok" {
		t.Errorf("a 1.2 s answer: %+v, overall %s", h.Parts, h.Overall)
	}
	f.delay.Store(0)
	f.notReady.Store(true)
	if h = sample(t, nil); partStatus(h, "workers") != "problem" {
		t.Errorf("a worker without its database: %+v", h.Parts)
	}
	f.notReady.Store(false)
	f.noModels.Store(true)
	if h = sample(t, nil); partStatus(h, "evidence") != "problem" || partStatus(h, "workers") != "ok" {
		t.Errorf("a worker without its models: %+v", h.Parts)
	}
}

// TestHealthHasNoHospitalOrUser: with hospitals, people and questions
// seeded, neither the stored reading nor the served one names any of them:
// no hospital id, name or code, no user id, name or email, no question id
// or text, and not the probe's own hospital id.
func TestHealthHasNoHospitalOrUser(t *testing.T) {
	clearHealth(t)
	f := startPipeline(t)
	var needles []string
	for i := 0; i < 3; i++ {
		hid := seedHospital(t, "active")
		var name, code string
		if err := super.QueryRow(context.Background(), `SELECT name, short_code FROM hospitals WHERE id = $1`, hid).Scan(&name, &code); err != nil {
			t.Fatal(err)
		}
		u := seedUser(t, hid, "Dr Synthetic Needle "+uniq(), authz.RoleClinician)
		q1 := seedQuestion(t, hid, u.id, "completed", "", time.Minute, 900, `{"privacy": 10, "answer": 800}`)
		q2 := seedQuestion(t, hid, u.id, "failed", "unavailable", time.Minute, 0, "")
		q3 := seedQuestion(t, hid, u.id, "queued", "", 0, 0, "")
		needles = append(needles, hid.String(), name, code, u.id.String(), u.name, u.email, q1.String(), q2.String(), q3.String())
	}
	reader := seedUser(t, uuid.Nil, "Ms Synthetic Reader "+uniq(), authz.RoleAuditor)
	needles = append(needles, reader.id.String(), reader.name, reader.email, "SYNTHETIC question", "synthetic patient")
	sample(t, nil)
	claims, _ := f.seen()
	if len(claims) == 0 {
		t.Fatal("no probe")
	}
	needles = append(needles, claims[0].Hid, claims[0].Rid)

	e, err := healthKV.Get(context.Background(), platform.HealthKey)
	if err != nil {
		t.Fatal(err)
	}
	r := get(t, "/platform/health", mint(t, reader))
	ok(t, "health", r)
	for _, where := range []struct{ name, text string }{{"stored", string(e.Value())}, {"served", r.raw}} {
		for _, n := range needles {
			if strings.Contains(strings.ToLower(where.text), strings.ToLower(n)) {
				t.Errorf("the %s reading contains %q", where.name, n)
			}
		}
		for _, key := range []string{`"hospital`, `"user`, `"patient`, `"name"`, `"email"`, `"id"`} {
			if strings.Contains(where.text, key) {
				t.Errorf("the %s reading has a key like %s", where.name, key)
			}
		}
	}
}

// TestSamplerStepsDownWhenItCantWrite (fix round 1): a leader whose
// readings keep failing (here its KV Put) steps down after two, and another
// copy writes within three ticks of starting; the failed one stays out.
func TestSamplerStepsDownWhenItCantWrite(t *testing.T) {
	clearHealth(t)
	startPipeline(t)
	const tick = 200 * time.Millisecond
	bad := &failingPut{KeyValue: healthKV}
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		cancelA()
		cancelB()
		<-doneA
		<-doneB
	})
	a := sampler(nil)
	a.KV = bad
	go func() {
		defer close(doneA)
		_ = a.Run(ctxA, tick)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for bad.puts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if bad.puts.Load() == 0 {
		close(doneB)
		t.Fatal("the failing sampler never led")
	}
	// a leads and has failed once; b starts now.
	var wrote atomic.Int32
	b := sampler(nil)
	b.Sampled = func(platform.HealthOut) { wrote.Add(1) }
	started := time.Now()
	go func() {
		defer close(doneB)
		_ = b.Run(ctxB, tick)
	}()
	// Three ticks, plus one reading's time.
	for wrote.Load() == 0 && time.Since(started) < 3*tick+300*time.Millisecond {
		time.Sleep(10 * time.Millisecond)
	}
	if wrote.Load() == 0 {
		t.Fatalf("no other sampler wrote within 3 ticks (the failing one tried %d puts)", bad.puts.Load())
	}
	if n := bad.puts.Load(); n != 2 {
		t.Errorf("the failing leader tried %d puts before stepping down, want 2", n)
	}
	time.Sleep(4 * tick)
	if n := bad.puts.Load(); n != 2 {
		t.Errorf("the failing sampler took the lock back from a working one (%d puts)", n)
	}
	ok(t, "health after the step-down", get(t, "/platform/health", mint(t, auditor(t))))
}

// TestHealthQueueCountsLearningJobs (fix round 1): a queued
// approve_correction job (the learning consumer's) counts in queue.waiting
// and its age in oldest_wait_s, as run_query questions do.
func TestHealthQueueCountsLearningJobs(t *testing.T) {
	clearHealth(t)
	startPipeline(t)
	ctx := context.Background()
	h := seedHospital(t, "active")
	u := seedUser(t, h, "", authz.RoleClinician)
	q := seedQuestion(t, h, u.id, "completed", "", time.Hour, 100, "")
	corr, job := uuid.New(), uuid.New()
	if _, err := super.Exec(ctx, `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis)
		VALUES ($1, $2, $3, $4, 'correct', 'pending_review', 'synthetic reason', 'synthetic diagnosis')`, corr, h, q, u.id); err != nil {
		t.Fatal(err)
	}
	waitingBefore := sample(t, nil).Queue.Waiting
	if _, err := super.Exec(ctx, `INSERT INTO jobs (id, hospital_id, type, subject_id, created_by, request_id, status, deadline_at, created_at)
		VALUES ($1, $2, 'approve_correction', $3, $4, 'req-synthetic', 'queued', now() + interval '5 minutes', now() - interval '20 minutes')`,
		job, h, corr, u.id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = super.Exec(context.Background(), `DELETE FROM jobs WHERE id = $1`, job) })
	got := sample(t, nil).Queue
	if got.Waiting != waitingBefore+1 {
		t.Errorf("waiting %d, want %d (the queued learning job)", got.Waiting, waitingBefore+1)
	}
	if got.OldestWaitS == nil || *got.OldestWaitS < 1190 {
		t.Errorf("oldest_wait_s %v, want the 20-minute-old learning job", got.OldestWaitS)
	}
	// Once it is running it no longer waits.
	if _, err := super.Exec(ctx, `UPDATE jobs SET status = 'running' WHERE id = $1`, job); err != nil {
		t.Fatal(err)
	}
	if n := sample(t, nil).Queue.Waiting; n != waitingBefore {
		t.Errorf("waiting %d with the job running, want %d", n, waitingBefore)
	}
}

// TestHealthSinceResetsAfterAGap (fix round 1): a part's since carries
// over only from a reading at most HealthMaxAge older; after a longer gap
// (nobody watched) it starts again at the new reading.
func TestHealthSinceResetsAfterAGap(t *testing.T) {
	clearHealth(t)
	startPipeline(t)
	base := time.Now()
	first := sample(t, func() time.Time { return base.Add(-4 * time.Minute) })
	near := sample(t, func() time.Time { return base.Add(-3 * time.Minute) })
	if near.Parts[0].Since != first.Parts[0].Since {
		t.Errorf("since moved within the max age: %s → %s", first.Parts[0].Since, near.Parts[0].Since)
	}
	late := sample(t, func() time.Time { return base })
	if late.Parts[0].Since != late.UpdatedAt {
		t.Errorf("since %s carried over a 3-minute gap (reading at %s)", late.Parts[0].Since, late.UpdatedAt)
	}
}

// TestSamplerDoesntLeadWithoutTimeouts (fix round 2): if the leader's
// keepalive and timeout settings can't be set, the copy gives the lock up
// at once and never samples; a copy that can set them leads at once.
func TestSamplerDoesntLeadWithoutTimeouts(t *testing.T) {
	clearHealth(t)
	startPipeline(t)
	const tick = 100 * time.Millisecond
	restore := platform.SetLeaderSession(`SELECT synthetic_no_such_function()`)
	var sampled atomic.Int32
	s := sampler(nil)
	s.Sampled = func(platform.HealthOut) { sampled.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx, tick)
	}()
	time.Sleep(5 * tick)
	cancel()
	<-done
	restore()
	if n := sampled.Load(); n != 0 {
		t.Fatalf("a copy without the session timeouts sampled %d times", n)
	}
	// The lock wasn't left held: a working copy leads on its first tick.
	var wrote atomic.Int32
	w := sampler(nil)
	w.Sampled = func(platform.HealthOut) { wrote.Add(1) }
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		_ = w.Run(ctx2, time.Hour)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for wrote.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel2()
	<-done2
	if wrote.Load() == 0 {
		t.Fatal("the lock was left held after the failed session setup")
	}
}
