//go:build integration

package queries_test

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/queries"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const (
	serviceKey = "test-only-bff-service-key-0123456789abcdef"
	testMFAKey = "test-only-mfa-key-0123456789abcdef0123456789"
	// A marker that must never leave the row: not in an audit payload, a
	// log line or a NATS message.
	marker = "SYNTHETIC-MARKER-7f3a"
)

var (
	super, app *pgxpool.Pool
	natsSrv    *testutil.NATS
	gwNC       *nats.Conn
	gwJS       jetstream.JetStream
	buckets    bus.Buckets
	issuer     token.Issuer
	signer     reqtoken.Signer
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
		if gwNC, gwJS, err = bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{}); err != nil {
			fmt.Fprintln(os.Stderr, "nats:", err)
			return 1
		}
		defer gwNC.Close()
		if buckets, err = bus.OpenBuckets(ctx, gwJS, 1); err != nil {
			fmt.Fprintln(os.Stderr, "buckets:", err)
			return 1
		}
		if err := bus.EnsureStreams(ctx, gwJS, bus.StreamConfig{Replicas: 1, PipelineMaxAckPending: 8}); err != nil {
			fmt.Fprintln(os.Stderr, "streams:", err)
			return 1
		}
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		issuer = token.Issuer{Key: priv, KeyID: "test-kid"}
		_, rk, _ := ed25519.GenerateKey(rand.Reader)
		signer = reqtoken.Signer{Key: rk, KeyID: reqtoken.KeyID(rk.Public().(ed25519.PublicKey))}
		return m.Run()
	}()
	os.Exit(code)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	h    http.Handler
	logs *syncBuf
}

// newEnv is core's whole chain (server.Register with the real guard and
// idempotency stores and the intelligence client) over HTTP, with an
// in-flight cap of limit.
func newEnv(t *testing.T, limit int) *env {
	t.Helper()
	e := &env{logs: &syncBuf{}}
	log := logx.New(e.logs)
	svc := &auth.Service{Pool: app, Buckets: buckets, Issuer: issuer, Keys: issuer.PublicKeys(), Log: log,
		MFAKey: []byte(testMFAKey), Argon: password.NewGate(64, time.Minute)}
	guard := &authz.Guard{Keys: issuer.PublicKeys(), Denylist: buckets.Denylist, Pool: app, Log: log,
		Idem: httpx.IdempotentLog(app, log), IdemAnon: httpx.IdempotentAnonLog(app, log)}
	m := authz.NewMux(guard)
	client := &intel.Client{NC: gwNC, JS: gwJS, Signer: signer, Log: log}
	server.Register(m, server.Deps{Auth: svc, Pool: app, Intel: client, QueryCap: limit})
	e.h = m.Handler([]string{serviceKey})
	return e
}

func uniq() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

func seedHospital(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic General', $1, 'active') RETURNING id`,
		"H"+uniq()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO hospital_settings (hospital_id) VALUES ($1)`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

type user struct {
	id, hid uuid.UUID
	roles   []string
}

func seedUser(t *testing.T, hid uuid.UUID, roles ...string) user {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name)
		VALUES ($1, $2, 'synthetic-hash', 'Dr Synthetic') RETURNING id`, hid, "synthetic."+uniq()+"@havana.example").Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := super.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled, backup_code_hashes)
		VALUES ($1, '\x00', true, ARRAY['a'])`, id); err != nil {
		t.Fatal(err)
	}
	return user{id: id, hid: hid, roles: roles}
}

func mint(t *testing.T, u user) string {
	t.Helper()
	sid := uuid.New()
	amr := []string{"pwd", "otp"}
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, true, $3)`, sid, u.id, amr); err != nil {
		t.Fatal(err)
	}
	at, _, err := issuer.Access(token.Claims{Sub: u.id.String(), Hid: u.hid.String(), Roles: u.roles, MFA: true, AMR: amr,
		Sid: sid.String(), Jti: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return at
}

type resp struct {
	code int
	body map[string]any
	raw  string
}

func (e *env) do(t *testing.T, method, path, tok, body string, hdr map[string]string) resp {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, httpx.BasePath+path, rd)
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("X-MHAI-Client-IP", "198.51.100.9")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	out := resp{code: w.Code, raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.body)
	return out
}

func question(text string) string {
	b, _ := json.Marshal(map[string]any{
		"patient_data":      map[string]any{"age": 54, "sex": "female", "symptoms": []string{"chest pain", marker}, "medical_history": "synthetic"},
		"clinical_question": text,
		"urgency":           "urgent",
		"specialty":         "cardiology",
	})
	return string(b)
}

// jobs reads and acks every waiting run_query job for hid, as the
// pipeline worker's NATS user on its durable.
func jobs(t *testing.T, hid uuid.UUID) []jetstream.Msg {
	t.Helper()
	nc, js, err := bus.Connect(context.Background(), natsSrv.PipelineURL, "", bus.WorkerInbox("pipeline"), bus.TLS{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	cons, err := js.Consumer(context.Background(), bus.JobsStream, bus.PipelineConsumer)
	if err != nil {
		t.Fatal(err)
	}
	var out []jetstream.Msg
	for {
		batch, err := cons.Fetch(16, jetstream.FetchMaxWait(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for m := range batch.Messages() {
			n++
			_ = m.Ack()
			if m.Subject() == "jobs.v1."+hid.String()+".run_query" {
				out = append(out, m)
			}
		}
		if n == 0 {
			return out
		}
	}
}

type row struct {
	status, urgency string
	category        *string
	createdBy       uuid.UUID
	requestID       *string
	published       bool
	deadline        time.Time
}

func rowOf(t *testing.T, id string) row {
	t.Helper()
	var r row
	if err := super.QueryRow(context.Background(), `SELECT status, urgency, category, created_by, request_id, published_at IS NOT NULL, deadline_at
		FROM clinical_queries WHERE id = $1`, id).Scan(&r.status, &r.urgency, &r.category, &r.createdBy, &r.requestID, &r.published, &r.deadline); err != nil {
		t.Fatal(err)
	}
	return r
}

func countRows(t *testing.T, hid uuid.UUID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM clinical_queries WHERE hospital_id = $1`, hid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// POST /queries: 202 {query_id, status: queued}; the row is queued with a
// 10-minute deadline and the request id; query.submitted is audited with
// ids and codes only (never the question or patient data); the pipeline
// receives one job whose token is for pipeline, this hospital and this
// user, carrying the query id only.
func TestPostQueries202AndAudited(t *testing.T) {
	e := newEnv(t, 10)
	hid := seedHospital(t)
	u := seedUser(t, hid, authz.RoleClinician)
	jobs(t, hid)
	r := e.do(t, "POST", "/queries", mint(t, u), question("Synthetic question "+marker+"?"), nil)
	if r.code != http.StatusAccepted || r.body["status"] != "queued" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	id, _ := r.body["query_id"].(string)
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("query_id %q", id)
	}
	got := rowOf(t, id)
	if got.status != "queued" || got.urgency != "urgent" || got.createdBy != u.id || got.requestID == nil || *got.requestID == "" ||
		got.category == nil || *got.category != "cardiology" {
		t.Fatalf("row %+v", got)
	}
	if d := time.Until(got.deadline); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("deadline in %v", d)
	}
	var actor, hosp string
	var payload []byte
	if err := super.QueryRow(context.Background(), `SELECT actor_id::text, hospital_id::text, payload FROM audit_log
		WHERE action = 'query.submitted' AND entity_id = $1`, id).Scan(&actor, &hosp, &payload); err != nil {
		t.Fatal(err)
	}
	var p map[string]any
	_ = json.Unmarshal(payload, &p)
	if actor != u.id.String() || hosp != hid.String() || p["query_id"] != id || p["urgency"] != "urgent" || len(p) != 2 {
		t.Fatalf("audit %s %s %v", actor, hosp, p)
	}
	if strings.Contains(string(payload), marker) || strings.Contains(e.logs.String(), marker) {
		t.Fatal("the question or patient data reached the audit or the logs")
	}
	ms := jobs(t, hid)
	if len(ms) != 1 {
		t.Fatalf("%d jobs", len(ms))
	}
	m := ms[0]
	if m.Headers().Get("Nats-Msg-Id") != id || strings.Contains(string(m.Data()), marker) {
		t.Fatalf("job msg id %q", m.Headers().Get("Nats-Msg-Id"))
	}
	var job intelpb.RunQueryJob
	if err := proto.Unmarshal(m.Data(), &job); err != nil || job.QueryId != id {
		t.Fatalf("job body %v %v", job.QueryId, err)
	}
	cl, err := reqtoken.VerifyAt(m.Headers().Get("Mhai-Request-Token"), signer.PublicKeys(),
		reqtoken.Expect{Aud: "pipeline", Subject: m.Subject(), Kind: reqtoken.KindJob}, m.Data(), time.Now())
	if err != nil || cl.Sub != u.id.String() || cl.Hid != hid.String() || cl.Kind != reqtoken.KindJob {
		t.Fatalf("token %+v %v", cl, err)
	}
	if !rowOf(t, id).published {
		t.Fatal("published_at not set after the publish")
	}
}

// The same Idempotency-Key twice: one row, one job, the same answer.
func TestPostQueriesIdempotent(t *testing.T) {
	e := newEnv(t, 10)
	hid := seedHospital(t)
	u := seedUser(t, hid, authz.RoleClinician)
	jobs(t, hid)
	tok := mint(t, u)
	key := map[string]string{"Idempotency-Key": "k-" + uuid.NewString()}
	body := question("Synthetic question?")
	a := e.do(t, "POST", "/queries", tok, body, key)
	b := e.do(t, "POST", "/queries", tok, body, key)
	if a.code != 202 || b.code != 202 || a.body["query_id"] != b.body["query_id"] {
		t.Fatalf("%d %s / %d %s", a.code, a.raw, b.code, b.raw)
	}
	if n := countRows(t, hid); n != 1 {
		t.Fatalf("%d rows", n)
	}
	if n := len(jobs(t, hid)); n != 1 {
		t.Fatalf("%d jobs", n)
	}
}

// A19 (endpoint): at the cap, hospital A gets 429 rate_limited and no row;
// hospital B still gets 202.
func TestPostQueriesAtCap429(t *testing.T) {
	e := newEnv(t, 2)
	a, b := seedHospital(t), seedHospital(t)
	ua, ub := seedUser(t, a, authz.RoleClinician), seedUser(t, b, authz.RoleClinician)
	ta, tb := mint(t, ua), mint(t, ub)
	for i := 0; i < 2; i++ {
		if r := e.do(t, "POST", "/queries", ta, question("Synthetic?"), nil); r.code != 202 {
			t.Fatalf("submit %d: %d %s", i, r.code, r.raw)
		}
	}
	r := e.do(t, "POST", "/queries", ta, question("Synthetic?"), nil)
	if r.code != http.StatusTooManyRequests || r.body["code"] != "rate_limited" {
		t.Fatalf("at cap: %d %s", r.code, r.raw)
	}
	if n := countRows(t, a); n != 2 {
		t.Fatalf("hospital A has %d rows", n)
	}
	if r := e.do(t, "POST", "/queries", tb, question("Synthetic?"), nil); r.code != 202 {
		t.Fatalf("hospital B: %d %s", r.code, r.raw)
	}
	jobs(t, a)
	jobs(t, b)
}

// Bad bodies are 422 validation_failed with the fixed body, echoing
// nothing, and write nothing. Only clinicians may ask.
func TestPostQueriesValidation(t *testing.T) {
	e := newEnv(t, 10)
	hid := seedHospital(t)
	tok := mint(t, seedUser(t, hid, authz.RoleClinician))
	good := map[string]any{
		"patient_data":      map[string]any{"age": 40, "sex": "male", "symptoms": []string{"cough"}},
		"clinical_question": "Synthetic?", "urgency": "routine",
	}
	with := func(f func(m map[string]any)) string {
		raw, _ := json.Marshal(good)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f(m)
		out, _ := json.Marshal(m)
		return string(out)
	}
	pd := func(m map[string]any) map[string]any { return m["patient_data"].(map[string]any) }
	many := make([]string, 51)
	for i := range many {
		many[i] = "s"
	}
	cases := map[string]string{
		"unknown field":   with(func(m map[string]any) { m["echo_"+marker] = 1 }),
		"long question":   with(func(m map[string]any) { m["clinical_question"] = strings.Repeat("q", 4001) }),
		"empty question":  with(func(m map[string]any) { m["clinical_question"] = "   " }),
		"urgency now":     with(func(m map[string]any) { m["urgency"] = "now" }),
		"age 131":         with(func(m map[string]any) { pd(m)["age"] = 131 }),
		"age fraction":    with(func(m map[string]any) { pd(m)["age"] = 4.5 }),
		"no age":          with(func(m map[string]any) { delete(pd(m), "age") }),
		"bad sex":         with(func(m map[string]any) { pd(m)["sex"] = "x" }),
		"51 symptoms":     with(func(m map[string]any) { pd(m)["symptoms"] = many }),
		"long symptom":    with(func(m map[string]any) { pd(m)["symptoms"] = []string{strings.Repeat("s", 201)} }),
		"no symptoms":     with(func(m map[string]any) { delete(pd(m), "symptoms") }),
		"unknown patient": with(func(m map[string]any) { pd(m)["mrn"] = marker }),
		"specialty digit": with(func(m map[string]any) { m["specialty"] = "cardio 1999" }),
		"bad client time": with(func(m map[string]any) { m["client_created_at"] = "yesterday" }),
		"no patient":      with(func(m map[string]any) { delete(m, "patient_data") }),
		"not json":        "{" + marker,
	}
	for name, body := range cases {
		r := e.do(t, "POST", "/queries", tok, body, nil)
		if r.code != 422 || r.body["code"] != "validation_failed" || strings.Contains(r.raw, marker) || strings.Contains(r.raw, "4001") {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
	if n := countRows(t, hid); n != 0 {
		t.Fatalf("refused bodies wrote %d rows", n)
	}
	ok := with(func(m map[string]any) {
		m["client_created_at"] = "2026-10-28T10:00:00Z"
		m["specialty"] = "General surgery"
	})
	if r := e.do(t, "POST", "/queries", tok, ok, nil); r.code != 202 {
		t.Fatalf("valid body: %d %s", r.code, r.raw)
	}
	admin := mint(t, seedUser(t, hid, authz.RoleHospitalAdmin))
	if r := e.do(t, "POST", "/queries", admin, ok, nil); r.code != 403 {
		t.Fatalf("hospital admin: %d", r.code)
	}
	jobs(t, hid)
}

func submit(t *testing.T, e *env, tok string) string {
	t.Helper()
	r := e.do(t, "POST", "/queries", tok, question("Synthetic?"), nil)
	if r.code != 202 {
		t.Fatalf("submit: %d %s", r.code, r.raw)
	}
	return r.body["query_id"].(string)
}

// Another hospital's question is 404 on both reads, never 403; so is an
// unknown or malformed id.
func TestGetQueryOtherHospital404(t *testing.T) {
	e := newEnv(t, 10)
	a, b := seedHospital(t), seedHospital(t)
	id := submit(t, e, mint(t, seedUser(t, a, authz.RoleClinician)))
	other := mint(t, seedUser(t, b, authz.RoleClinician))
	for _, p := range []string{"/queries/" + id, "/queries/" + id + "/result", "/queries/" + uuid.NewString(), "/queries/not-a-uuid",
		"/queries/" + strings.ToUpper(id)} {
		if r := e.do(t, "GET", p, other, "", nil); r.code != 404 || r.body["code"] != "not_found" {
			t.Errorf("%s: %d %s", p, r.code, r.raw)
		}
	}
	jobs(t, a)
}

// Another clinician of the same hospital can't read the question either.
func TestGetQueryOtherClinician404(t *testing.T) {
	e := newEnv(t, 10)
	hid := seedHospital(t)
	id := submit(t, e, mint(t, seedUser(t, hid, authz.RoleClinician)))
	colleague := mint(t, seedUser(t, hid, authz.RoleClinician))
	for _, p := range []string{"/queries/" + id, "/queries/" + id + "/result"} {
		if r := e.do(t, "GET", p, colleague, "", nil); r.code != 404 {
			t.Errorf("%s: %d %s", p, r.code, r.raw)
		}
	}
	jobs(t, hid)
}

// Departure 8: running shows as processing, cancelled as failed with
// error_code cancelled, failed with its code; the result is 404 until the
// question is completed, then the contract's shape.
func TestStatusMapping(t *testing.T) {
	e := newEnv(t, 10)
	hid := seedHospital(t)
	tok := mint(t, seedUser(t, hid, authz.RoleClinician))
	id := submit(t, e, tok)
	set := func(s string) {
		if _, err := super.Exec(context.Background(), `UPDATE clinical_queries SET `+s+` WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	get := func() resp { return e.do(t, "GET", "/queries/"+id, tok, "", nil) }
	if r := get(); r.code != 200 || r.body["status"] != "queued" || r.body["current_stage"] != nil || r.body["error_code"] != nil {
		t.Fatalf("queued: %s", r.raw)
	}
	set(`status = 'running', progress = 0.5, current_stage = 'retrieval'`)
	if r := get(); r.body["status"] != "processing" || r.body["progress"] != 0.5 || r.body["current_stage"] != "retrieval" {
		t.Fatalf("running: %s", r.raw)
	}
	if r := e.do(t, "GET", "/queries/"+id+"/result", tok, "", nil); r.code != 404 {
		t.Fatalf("result while running: %d", r.code)
	}
	set(`status = 'cancelled', cancel_reason = 'user_deactivated'`)
	if r := get(); r.body["status"] != "failed" || r.body["error_code"] != "cancelled" {
		t.Fatalf("cancelled: %s", r.raw)
	}
	set(`status = 'failed', error = 'deadline_exceeded', cancel_reason = NULL`)
	if r := get(); r.body["status"] != "failed" || r.body["error_code"] != "deadline_exceeded" {
		t.Fatalf("failed: %s", r.raw)
	}
	set(`status = 'running', error = NULL`)
	if _, err := super.Exec(context.Background(), `INSERT INTO clinical_decisions (id, hospital_id, query_id, response_text, citations_json,
		trust_score, hga_decision, guardrail_report_json, retrieval_ms, generation_ms, verified, degraded, failed_agents, model_versions, trust_band)
		VALUES ($1, $2, $3, 'Synthetic answer', '["chunk-1","chunk-2"]', 0.82, 'PASS', '{}', 12, 340, true, false, '{}', '{"cca":"1"}', 'high')`,
		uuid.New(), hid, id); err != nil {
		t.Fatal(err)
	}
	set(`status = 'completed', progress = 1, current_stage = 'decision'`)
	if r := get(); r.body["status"] != "completed" {
		t.Fatalf("completed: %s", r.raw)
	}
	r := e.do(t, "GET", "/queries/"+id+"/result", tok, "", nil)
	if r.code != 200 || r.body["decision"] != "PASS" || r.body["verified"] != true || r.body["degraded"] != false ||
		r.body["trust_band"] != "high" || r.body["response_text"] != "Synthetic answer" || r.body["response_available"] != true {
		t.Fatalf("result: %d %s", r.code, r.raw)
	}
	for _, k := range []string{"query_id", "failed_agents", "diagnosis", "icd10_code", "trust_score", "citations", "guardrail_details",
		"retrieval_ms", "generation_ms", "cited_chunk_ids"} {
		if _, ok := r.body[k]; !ok {
			t.Errorf("result lacks %s", k)
		}
	}
	if ids, _ := r.body["cited_chunk_ids"].([]any); len(ids) != 2 {
		t.Errorf("cited_chunk_ids %v", r.body["cited_chunk_ids"])
	}
	jobs(t, hid)
}

// A question submitted after its creator was deactivated (the principal
// cache may still hold the account) is refused in the submit transaction
// itself and writes nothing: no row can slip past the cancel trigger.
func TestPostQueriesRefusesDeactivatedAccount(t *testing.T) {
	e := newEnv(t, 10)
	hid := seedHospital(t)
	u := seedUser(t, hid, authz.RoleClinician)
	tok := mint(t, u)
	// Warm the guard's account cache.
	if r := e.do(t, "GET", "/queries/"+uuid.NewString(), tok, "", nil); r.code != 404 {
		t.Fatalf("warm-up: %d %s", r.code, r.raw)
	}
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u.id); err != nil {
		t.Fatal(err)
	}
	if r := e.do(t, "POST", "/queries", tok, question("Synthetic?"), nil); r.code != http.StatusUnauthorized {
		t.Fatalf("deactivated: %d %s", r.code, r.raw)
	}
	if n := countRows(t, hid); n != 0 {
		t.Fatalf("%d rows for a deactivated account", n)
	}
}

// Final review I-3: a hospital deactivated while a question is submitted.
// The deactivation's trigger (00009) ran before the question's row
// committed, so it couldn't see the row to cancel it, and committed before
// the publish. The question must not be published: the row stays queued
// and unpublished for the sweeper (which cancels it, account_unusable), and
// no job reaches the pipeline.
func TestPostQueriesHospitalDeactivatedDuringSubmit(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, 10)
	hid := seedHospital(t)
	u := seedUser(t, hid, authz.RoleClinician)
	tok := mint(t, u)
	jobs(t, hid)
	deact, err := super.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deact.Rollback(ctx) }()
	if _, err := deact.Exec(ctx, `UPDATE hospitals SET status = 'deactivated' WHERE id = $1`, hid); err != nil {
		t.Fatal(err)
	}
	restore := queries.SetAfterCommit(func() {
		if err := deact.Commit(ctx); err != nil {
			t.Errorf("deactivation commit: %v", err)
		}
	})
	defer restore()
	r := e.do(t, "POST", "/queries", tok, question("Synthetic?"), nil)
	if r.code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", r.code, r.raw)
	}
	id, _ := r.body["query_id"].(string)
	if got := rowOf(t, id); got.status != "queued" || got.published {
		t.Fatalf("row after the race: %+v", got)
	}
	if js := jobs(t, hid); len(js) != 0 {
		t.Fatalf("%d jobs published for a deactivated hospital", len(js))
	}
}
