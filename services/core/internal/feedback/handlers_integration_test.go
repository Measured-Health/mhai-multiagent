//go:build integration

package feedback_test

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
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intel"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// Plan 4 Task 5: POST /feedback and the corrections routes through core's
// whole chain (spec §4.2, §4.4, §9; abuse cases C1, C3-C7, C11, C12, C14,
// C16). Every name, id and string here is synthetic.

const (
	serviceKey = "test-only-bff-service-key-0123456789abcdef"
	testMFAKey = "test-only-mfa-key-0123456789abcdef0123456789"
	// A marker that must never leave the feedback row's free text.
	marker = "SYNTHETIC-FEEDBACK-MARKER-5d1e"
	// The synthetic patient the stored (unmasked) question names (C14).
	patientName = "Zebedee Quist"
	patientMRN  = "MRN-SYN-0001"
)

var (
	super, app *pgxpool.Pool
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
	h      http.Handler
	logs   *syncBuf
	js     *recJS
	client *intel.Client
}

// newEnv is core's whole chain: server.Register with the real guard,
// idempotency store and intelligence client.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{logs: &syncBuf{}}
	log := logx.New(e.logs)
	svc := &auth.Service{Pool: app, Buckets: buckets, Issuer: issuer, Keys: issuer.PublicKeys(), Log: log,
		MFAKey: []byte(testMFAKey), Argon: password.NewGate(64, time.Minute)}
	guard := &authz.Guard{Keys: issuer.PublicKeys(), Denylist: buckets.Denylist, Pool: app, Log: log,
		Idem: httpx.IdempotentLog(app, log), IdemAnon: httpx.IdempotentAnonLog(app, log)}
	m := authz.NewMux(guard)
	e.js = &recJS{JetStream: gwJS}
	e.client = &intel.Client{NC: gwNC, JS: e.js, Signer: signer, Log: log, Pool: app}
	server.Register(m, server.Deps{Auth: svc, Pool: app, Intel: e.client, QueryCap: 10})
	e.h = m.Handler([]string{serviceKey})
	return e
}

func uniq() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

const hospitalName = "Synthetic General"

func seedHospital(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ($1, $2, 'active') RETURNING id`,
		hospitalName, "H"+uniq()).Scan(&id); err != nil {
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
	name    string
}

// seedUser adds a user of hospital hid (uuid.Nil: a platform account).
func seedUser(t *testing.T, hid uuid.UUID, roles ...string) user {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	var h any
	if hid != uuid.Nil {
		h = hid
	}
	name := "Dr Synthetic " + uniq()
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, full_name, specialty)
		VALUES ($1, $2, 'synthetic-hash', $3, 'Synthetic Fullname', 'cardiology') RETURNING id`,
		h, "synthetic."+uniq()+"@havana.example", name).Scan(&id); err != nil {
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
	return user{id: id, hid: hid, roles: roles, name: name}
}

func clinician(t *testing.T, hid uuid.UUID) user { return seedUser(t, hid, authz.RoleClinician) }

func mint(t *testing.T, u user) string {
	t.Helper()
	sid := uuid.New()
	amr := []string{"pwd", "otp"}
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, true, $3)`, sid, u.id, amr); err != nil {
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

type resp struct {
	code int
	body map[string]any
	list []any
	raw  string
}

func (e *env) do(t *testing.T, method, path, tok, body string, hdr map[string]string) resp {
	t.Helper()
	r := httptest.NewRequest(method, httpx.BasePath+path, strings.NewReader(body))
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
	_ = json.Unmarshal(w.Body.Bytes(), &out.list)
	return out
}

// seedQuery adds a completed question asked by u, with its decision; the
// masked copy only when masked is true. The stored (raw) question and
// patient data name a synthetic patient; the masked copy has placeholders.
func seedQuery(t *testing.T, u user, masked bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	patient := fmt.Sprintf(`{"age": 61, "sex": "male", "symptoms": ["dyspnoea", "%s"], "medical_history": "%s"}`, patientName, patientMRN)
	if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, patient_data_json, urgency, category, status, progress, created_by)
		VALUES ($1, $2, $3, $4::jsonb, 'urgent', 'cardiology', 'completed', 1, $5)`,
		id, u.hid, "Does "+patientName+" ("+patientMRN+") need anticoagulation?", patient, u.id); err != nil {
		t.Fatal(err)
	}
	var mq, mp any
	if masked {
		mq = "Does [NAME] ([MRN]) need anticoagulation?"
		mp = `{"age": 61, "sex": "male", "symptoms": ["dyspnoea", "[NAME]"], "medical_history": "[MRN]"}`
	}
	if _, err := super.Exec(ctx, `INSERT INTO clinical_decisions (id, hospital_id, query_id, diagnosis, icd10_code, response_text, citations_json,
		trust_score, hga_decision, verified, degraded, failed_agents, masked_question, masked_patient_json)
		VALUES ($1, $2, $3, 'Synthetic atrial fibrillation', 'I48.9', 'Synthetic answer', '["KB#0001"]', 0.8, 'PASS', true, false, '{}', $4, $5::jsonb)`,
		uuid.New(), u.hid, id, mq, mp); err != nil {
		t.Fatal(err)
	}
	return id
}

func correctBody(q uuid.UUID, diagnosis, reason string) string {
	b, _ := json.Marshal(map[string]any{"query_id": q.String(), "action": "correct", "corrected_diagnosis": diagnosis, "reason": reason})
	return string(b)
}

func validateBody(q uuid.UUID) string {
	return `{"query_id":"` + q.String() + `","action":"validate"}`
}

// submitCorrection posts a correction for a fresh question of u and returns
// its id.
func submitCorrection(t *testing.T, e *env, u user) uuid.UUID {
	t.Helper()
	r := e.do(t, "POST", "/feedback", mint(t, u), correctBody(seedQuery(t, u, true), "Synthetic flutter", "Synthetic reason for review"), nil)
	if r.code != 200 || r.body["status"] != "pending_review" {
		t.Fatalf("submit: %d %s", r.code, r.raw)
	}
	id, err := uuid.Parse(fmt.Sprint(r.body["correction_id"]))
	if err != nil {
		t.Fatalf("correction_id %v", r.body["correction_id"])
	}
	return id
}

type fbRow struct {
	status    string
	decidedBy *uuid.UUID
	decidedAt *time.Time
	code      *string
}

func rowOf(t *testing.T, id uuid.UUID) fbRow {
	t.Helper()
	var r fbRow
	if err := super.QueryRow(context.Background(), `SELECT status, decided_by, decided_at, rejection_code FROM feedback_events WHERE id = $1`, id).
		Scan(&r.status, &r.decidedBy, &r.decidedAt, &r.code); err != nil {
		t.Fatal(err)
	}
	return r
}

type auditRow struct {
	action, actor, entity, entityID string
	payload                         map[string]any
}

func auditFor(t *testing.T, hid uuid.UUID, action string) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, COALESCE(actor_id::text, ''), COALESCE(entity, ''), COALESCE(entity_id, ''), payload
		FROM audit_log WHERE hospital_id = $1 AND ($2 = '' OR action = $2) ORDER BY id`, hid, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var raw []byte
		if err := rows.Scan(&a.action, &a.actor, &a.entity, &a.entityID, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func jobsFor(t *testing.T, subject uuid.UUID) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM jobs WHERE subject_id = $1`, subject).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameKeys(t *testing.T, what string, m map[string]any, want ...string) {
	t.Helper()
	sort.Strings(want)
	if got := keys(m); !slices.Equal(got, want) {
		t.Errorf("%s keys %v, want %v", what, got, want)
	}
}

func problem(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.code != status || r.body["code"] != code {
		t.Fatalf("want %d %s, got %d %s", status, code, r.code, r.raw)
	}
}

// --- POST /feedback ----------------------------------------------------------

// The shapes are exactly FeedbackOut; the audit rows are feedback.validated
// and correction.submitted with ids only.
func TestSubmitValidateAndCorrect(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	u := clinician(t, hid)
	tok := mint(t, u)

	q1 := seedQuery(t, u, true)
	r := e.do(t, "POST", "/feedback", tok, validateBody(q1), map[string]string{"Idempotency-Key": "k-" + uniq()})
	if r.code != 200 || r.body["status"] != "recorded" {
		t.Fatalf("validate: %d %s", r.code, r.raw)
	}
	sameKeys(t, "validate reply", r.body, "status")

	q2 := seedQuery(t, u, true)
	r = e.do(t, "POST", "/feedback", tok, correctBody(q2, "  Synthetic flutter  ", "  Synthetic reason  "), nil)
	if r.code != 200 || r.body["status"] != "pending_review" {
		t.Fatalf("correct: %d %s", r.code, r.raw)
	}
	sameKeys(t, "correct reply", r.body, "status", "correction_id")
	cid := uuid.MustParse(r.body["correction_id"].(string))

	var sub uuid.UUID
	var status, diag, reason string
	var rid *string
	if err := super.QueryRow(context.Background(), `SELECT submitted_by, status, corrected_diagnosis, reason, request_id FROM feedback_events WHERE id = $1`, cid).
		Scan(&sub, &status, &diag, &reason, &rid); err != nil {
		t.Fatal(err)
	}
	if sub != u.id || status != "pending_review" || diag != "Synthetic flutter" || reason != "Synthetic reason" || rid == nil || *rid == "" {
		t.Fatalf("row: %v %s %q %q %v", sub, status, diag, reason, rid)
	}

	v := auditFor(t, hid, "feedback.validated")
	if len(v) != 1 || v[0].actor != u.id.String() || v[0].entity != "query" || v[0].entityID != q1.String() {
		t.Fatalf("feedback.validated rows %+v", v)
	}
	sameKeys(t, "feedback.validated payload", v[0].payload, "query_id")
	if v[0].payload["query_id"] != q1.String() {
		t.Errorf("payload %v", v[0].payload)
	}
	c := auditFor(t, hid, "correction.submitted")
	if len(c) != 1 || c[0].actor != u.id.String() || c[0].entity != "correction" || c[0].entityID != cid.String() {
		t.Fatalf("correction.submitted rows %+v", c)
	}
	sameKeys(t, "correction.submitted payload", c[0].payload, "correction_id", "query_id")
	if c[0].payload["correction_id"] != cid.String() || c[0].payload["query_id"] != q2.String() {
		t.Errorf("payload %v", c[0].payload)
	}
}

// C11: another clinician's question, another hospital's, an unknown id and
// an unfinished one are all 404, and nothing is written.
func TestSubmitOnOthersQueryIs404(t *testing.T) {
	e := newEnv(t)
	hid, other := seedHospital(t), seedHospital(t)
	u := clinician(t, hid)
	tok := mint(t, u)
	colleague := seedQuery(t, clinician(t, hid), true)
	elsewhere := seedQuery(t, clinician(t, other), true)
	unfinished := seedQuery(t, u, true)
	if _, err := super.Exec(context.Background(), `UPDATE clinical_queries SET status = 'running' WHERE id = $1`, unfinished); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]uuid.UUID{"colleague's": colleague, "other hospital's": elsewhere, "unknown": uuid.New(), "unfinished": unfinished} {
		for _, body := range []string{validateBody(q), correctBody(q, "Synthetic", "Synthetic reason")} {
			problem(t, e.do(t, "POST", "/feedback", tok, body, nil), 404, "not_found")
		}
		// 404 comes before body validation.
		problem(t, e.do(t, "POST", "/feedback", tok, `{"query_id":"`+q.String()+`","action":"approve"}`, nil), 404, "not_found")
		_ = name
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM feedback_events WHERE submitted_by = $1`, u.id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows written, %v", n, err)
	}
}

// C11: one live feedback per question; after a withdrawal a new correction
// is allowed.
func TestSecondFeedbackIs409(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	u := clinician(t, hid)
	tok := mint(t, u)
	q := seedQuery(t, u, true)
	if r := e.do(t, "POST", "/feedback", tok, validateBody(q), nil); r.code != 200 {
		t.Fatalf("first: %d %s", r.code, r.raw)
	}
	problem(t, e.do(t, "POST", "/feedback", tok, validateBody(q), nil), 409, "conflict")
	problem(t, e.do(t, "POST", "/feedback", tok, correctBody(q, "Synthetic", "Synthetic reason"), nil), 409, "conflict")
	// 409 comes before body validation.
	problem(t, e.do(t, "POST", "/feedback", tok, correctBody(q, "", "x"), nil), 409, "conflict")

	q2 := seedQuery(t, u, true)
	r := e.do(t, "POST", "/feedback", tok, correctBody(q2, "Synthetic", "Synthetic reason"), nil)
	if r.code != 200 {
		t.Fatalf("correct: %d %s", r.code, r.raw)
	}
	cid := r.body["correction_id"].(string)
	problem(t, e.do(t, "POST", "/feedback", tok, validateBody(q2), nil), 409, "conflict")
	if w := e.do(t, "POST", "/corrections/"+cid+"/withdraw", tok, "", nil); w.code != 204 {
		t.Fatalf("withdraw: %d %s", w.code, w.raw)
	}
	if r := e.do(t, "POST", "/feedback", tok, correctBody(q2, "Synthetic again", "Synthetic reason"), nil); r.code != 200 {
		t.Fatalf("after withdrawal: %d %s", r.code, r.raw)
	}
}

// Spec §4.4: a correction needs the masked copy a reviewer may read.
func TestSubmitWithoutMaskedCopyIs409(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	u := clinician(t, hid)
	tok := mint(t, u)
	q := seedQuery(t, u, false)
	problem(t, e.do(t, "POST", "/feedback", tok, correctBody(q, "Synthetic", "Synthetic reason"), nil), 409, "conflict")
	// Agreeing needs no reviewer, so no masked copy.
	if r := e.do(t, "POST", "/feedback", tok, validateBody(q), nil); r.code != 200 {
		t.Fatalf("validate without a masked copy: %d %s", r.code, r.raw)
	}
}

func TestSubmitValidation(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	u := clinician(t, hid)
	tok := mint(t, u)
	q := seedQuery(t, u, true)
	qs := q.String()
	soon := time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	for name, body := range map[string]string{
		"reason too short":          correctBody(q, "Synthetic", "ab"),
		"reason short once trimmed": correctBody(q, "Synthetic", "  ab  "),
		"reason too long":           correctBody(q, "Synthetic", strings.Repeat("r", 1001)),
		"empty diagnosis":           correctBody(q, "   ", "Synthetic reason"),
		"diagnosis too long":        correctBody(q, strings.Repeat("d", 201), "Synthetic reason"),
		"NUL in reason":             correctBody(q, "Synthetic", "Synthetic\x00reason"),
		"bell in reason":            correctBody(q, "Synthetic", "Synthetic\x07reason"),
		"escape in diagnosis":       correctBody(q, "Synth\x1b[31m", "Synthetic reason"),
		"DEL in diagnosis":          correctBody(q, "Synth\x7f", "Synthetic reason"),
		"unknown action":            `{"query_id":"` + qs + `","action":"approve"}`,
		"no action":                 `{"query_id":"` + qs + `"}`,
		"validate with a reason":    `{"query_id":"` + qs + `","action":"validate","reason":"Synthetic reason"}`,
		"correct with no reason":    `{"query_id":"` + qs + `","action":"correct","corrected_diagnosis":"Synthetic"}`,
		"client time ahead":         `{"query_id":"` + qs + `","action":"validate","client_created_at":"` + soon + `"}`,
		"client time not a date":    `{"query_id":"` + qs + `","action":"validate","client_created_at":"yesterday"}`,
		"identity in the body":      `{"query_id":"` + qs + `","action":"validate","submitted_by":"` + uuid.NewString() + `"}`,
		"hospital in the body":      `{"query_id":"` + qs + `","action":"validate","hospital_id":"` + uuid.NewString() + `"}`,
		"not json":                  `{"query_id":`,
		"query id not a uuid":       `{"query_id":"q-1","action":"validate"}`,
	} {
		r := e.do(t, "POST", "/feedback", tok, body, nil)
		if r.code != 422 || r.body["code"] != "validation_failed" {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM feedback_events WHERE query_id = $1`, q).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d rows after refusals, %v", n, err)
	}
	// The bounds themselves pass; a client time 4 minutes ahead is fine.
	ahead := time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339)
	b, _ := json.Marshal(map[string]any{"query_id": qs, "action": "correct", "corrected_diagnosis": strings.Repeat("d", 200),
		"reason": strings.Repeat("r", 1000), "client_created_at": ahead})
	if r := e.do(t, "POST", "/feedback", tok, string(b), nil); r.code != 200 {
		t.Fatalf("bounds: %d %s", r.code, r.raw)
	}
	var cca *time.Time
	if err := super.QueryRow(context.Background(), `SELECT client_created_at FROM feedback_events WHERE query_id = $1`, q).Scan(&cca); err != nil || cca == nil {
		t.Fatalf("client_created_at not stored: %v %v", cca, err)
	}
}

// --- deciding ---------------------------------------------------------------

// C1: approving (or rejecting) your own correction is 403 self_approval,
// audited as correction.decision_refused, with no job.
func TestSelfApprovalRefusedAndAudited(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	u := clinician(t, hid)
	tok := mint(t, u)
	cid := submitCorrection(t, e, u)
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, "", nil), 403, "self_approval")
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/reject", tok, `{"reason_code":"unsafe"}`, nil), 403, "self_approval")
	// 403 comes before body validation.
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/reject", tok, `{"reason_code":"bogus"}`, nil), 403, "self_approval")
	if r := rowOf(t, cid); r.status != "pending_review" || r.decidedBy != nil {
		t.Fatalf("row %+v", r)
	}
	if n := jobsFor(t, cid); n != 0 {
		t.Fatalf("%d jobs", n)
	}
	refused := auditFor(t, hid, "correction.decision_refused")
	if len(refused) != 3 {
		t.Fatalf("%d refusal rows", len(refused))
	}
	for i, want := range []string{"approve", "reject", "reject"} {
		a := refused[i]
		sameKeys(t, "refusal payload", a.payload, "correction_id", "reason", "attempted")
		if a.actor != u.id.String() || a.entity != "correction" || a.entityID != cid.String() ||
			a.payload["reason"] != "self_approval" || a.payload["attempted"] != want || a.payload["correction_id"] != cid.String() {
			t.Errorf("refusal %d: %+v", i, a)
		}
	}
	if n := len(auditFor(t, hid, "correction.approved")); n != 0 {
		t.Fatalf("%d approvals audited", n)
	}
}

// C3: two sessions of the submitter and one of a reviewer approve at once:
// exactly one approval, by the reviewer.
func TestParallelApprovals(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	for round := 0; round < 5; round++ {
		cid := submitCorrection(t, e, sub)
		toks := []string{mint(t, sub), mint(t, sub), mint(t, rev)}
		codes := make([]int, len(toks))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, tok := range toks {
			wg.Add(1)
			go func(i int, tok string) {
				defer wg.Done()
				<-start
				codes[i] = e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, "", nil).code
			}(i, tok)
		}
		close(start)
		wg.Wait()
		if codes[2] != 204 {
			t.Fatalf("round %d: reviewer got %d (submitters %v)", round, codes[2], codes[:2])
		}
		for _, c := range codes[:2] {
			if c != 403 && c != 409 {
				t.Fatalf("round %d: submitter got %d", round, c)
			}
		}
		if r := rowOf(t, cid); r.status != "approved" || r.decidedBy == nil || *r.decidedBy != rev.id {
			t.Fatalf("round %d: row %+v", round, r)
		}
	}
	if n := len(auditFor(t, hid, "correction.approved")); n != 5 {
		t.Fatalf("%d approvals audited, want 5", n)
	}
}

// C4: the decider is the token's user; a body can't name another.
func TestApproveBodyCannotNameDecider(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	someone := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	tok := mint(t, rev)
	r := e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, `{"decided_by":"`+someone.id.String()+`"}`, nil)
	switch r.code {
	case 422:
		if row := rowOf(t, cid); row.status != "pending_review" {
			t.Fatalf("a refused body changed the row: %+v", row)
		}
		if r = e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, "", nil); r.code != 204 {
			t.Fatalf("approve: %d %s", r.code, r.raw)
		}
	case 204:
	default:
		t.Fatalf("approve with a forged body: %d %s", r.code, r.raw)
	}
	if row := rowOf(t, cid); row.decidedBy == nil || *row.decidedBy != rev.id {
		t.Fatalf("decided_by %v, want the token's user %v", row.decidedBy, rev.id)
	}
	a := auditFor(t, hid, "correction.approved")
	if len(a) != 1 || a[0].actor != rev.id.String() || a[0].entityID != cid.String() {
		t.Fatalf("approved rows %+v", a)
	}
	sameKeys(t, "correction.approved payload", a[0].payload, "correction_id", "job_id")
	if !audit_uuid(a[0].payload["job_id"]) || a[0].payload["correction_id"] != cid.String() {
		t.Fatalf("payload %v", a[0].payload)
	}
}

func audit_uuid(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// C5: a hospital admin who isn't a clinician, an analyst, a platform admin
// and an auditor get 403 on every corrections route.
func TestNonCliniciansRefused(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	cid := submitCorrection(t, e, clinician(t, hid))
	for name, u := range map[string]user{
		"hospital admin": seedUser(t, hid, authz.RoleHospitalAdmin),
		"analyst":        seedUser(t, hid, authz.RoleAnalyst),
		"platform admin": seedUser(t, uuid.Nil, authz.RolePlatformAdmin),
		"auditor":        seedUser(t, uuid.Nil, authz.RoleAuditor),
	} {
		tok := mint(t, u)
		for _, c := range []struct{ method, path, body string }{
			{"POST", "/corrections/" + cid.String() + "/approve", ""},
			{"POST", "/corrections/" + cid.String() + "/reject", `{"reason_code":"unsafe"}`},
			{"POST", "/corrections/" + cid.String() + "/withdraw", ""},
			{"GET", "/corrections?status=pending", ""},
			{"GET", "/corrections/mine", ""},
			{"GET", "/corrections/reviewed", ""},
			{"GET", "/feedback/stats/mine", ""},
			{"POST", "/feedback", validateBody(uuid.New())},
		} {
			if r := e.do(t, c.method, c.path, tok, c.body, nil); r.code != 403 {
				t.Errorf("%s %s %s: %d %s", name, c.method, c.path, r.code, r.raw)
			}
		}
	}
	if r := rowOf(t, cid); r.status != "pending_review" {
		t.Fatalf("row %+v", r)
	}
}

// C6: a reviewer whose clinician role is removed after loading the queue
// (the principal cache still holds it) is refused by the decider trigger:
// 409 conflict, nothing decided, no job. A deactivated reviewer is 401.
func TestDeciderLostRoleBetweenLoadAndApprove(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	tok := mint(t, rev)
	if r := e.do(t, "GET", "/corrections?status=pending", tok, "", nil); r.code != 200 {
		t.Fatalf("load: %d %s", r.code, r.raw)
	}
	if _, err := super.Exec(context.Background(), `DELETE FROM user_roles WHERE user_id = $1 AND role = 'clinician'`, rev.id); err != nil {
		t.Fatal(err)
	}
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/approve", tok, "", nil), 409, "conflict")
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/reject", tok, `{"reason_code":"unsafe"}`, nil), 409, "conflict")
	if r := rowOf(t, cid); r.status != "pending_review" || r.decidedBy != nil {
		t.Fatalf("row %+v", r)
	}
	if n := jobsFor(t, cid); n != 0 {
		t.Fatalf("%d jobs", n)
	}
	if n := len(auditFor(t, hid, "correction.approved")) + len(auditFor(t, hid, "correction.rejected")); n != 0 {
		t.Fatalf("%d decisions audited", n)
	}
	if strings.Contains(e.logs.String(), "the decider must be") {
		t.Fatal("the trigger's message reached the logs")
	}

	gone := clinician(t, hid)
	gtok := mint(t, gone)
	if r := e.do(t, "GET", "/corrections?status=pending", gtok, "", nil); r.code != 200 {
		t.Fatalf("load: %d %s", r.code, r.raw)
	}
	if _, err := super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, gone.id); err != nil {
		t.Fatal(err)
	}
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/approve", gtok, "", nil), 401, "unauthenticated")
	if r := rowOf(t, cid); r.status != "pending_review" {
		t.Fatalf("row %+v", r)
	}
}

// C7: hospital B's clinician gets 404 on hospital A's correction, on every
// route, and never sees it listed.
func TestOtherHospitalIs404(t *testing.T) {
	e := newEnv(t)
	a, b := seedHospital(t), seedHospital(t)
	cid := submitCorrection(t, e, clinician(t, a))
	tok := mint(t, clinician(t, b))
	for _, c := range []struct{ path, body string }{
		{"/corrections/" + cid.String() + "/approve", ""},
		{"/corrections/" + cid.String() + "/reject", `{"reason_code":"unsafe"}`},
		{"/corrections/" + cid.String() + "/withdraw", ""},
		{"/corrections/not-a-uuid/approve", ""},
		{"/corrections/" + strings.ToUpper(cid.String()) + "/approve", ""},
	} {
		problem(t, e.do(t, "POST", c.path, tok, c.body, nil), 404, "not_found")
	}
	for _, p := range []string{"/corrections?status=pending", "/corrections", "/corrections/mine", "/corrections/reviewed"} {
		r := e.do(t, "GET", p, tok, "", nil)
		if r.code != 200 || strings.Contains(r.raw, cid.String()) {
			t.Errorf("%s: %d %s", p, r.code, r.raw)
		}
	}
	if r := rowOf(t, cid); r.status != "pending_review" {
		t.Fatalf("row %+v", r)
	}
	if n := len(auditFor(t, b, "")); n != 0 {
		t.Fatalf("%d audit rows in hospital B for a 404", n)
	}
}

// C12: withdraw then approve is 409; approve then withdraw is 409. A
// colleague can't withdraw (403 forbidden, audited).
func TestWithdrawThenApprove409(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/withdraw", mint(t, rev), "", nil), 403, "forbidden")
	if r := e.do(t, "POST", "/corrections/"+cid.String()+"/withdraw", mint(t, sub), "", nil); r.code != 204 {
		t.Fatalf("withdraw: %d %s", r.code, r.raw)
	}
	if r := rowOf(t, cid); r.status != "withdrawn" || r.decidedBy != nil || r.decidedAt == nil {
		t.Fatalf("row %+v", r)
	}
	rtok := mint(t, rev)
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/approve", rtok, "", nil), 409, "already_decided")
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/reject", rtok, `{"reason_code":"unsafe"}`, nil), 409, "already_decided")
	// 409 comes before body validation.
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/reject", rtok, `{"reason_code":"bogus"}`, nil), 409, "already_decided")
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/withdraw", mint(t, sub), "", nil), 409, "already_decided")
	if n := jobsFor(t, cid); n != 0 {
		t.Fatalf("%d jobs", n)
	}
	w := auditFor(t, hid, "correction.withdrawn")
	if len(w) != 1 || w[0].actor != sub.id.String() {
		t.Fatalf("withdrawn rows %+v", w)
	}
	sameKeys(t, "correction.withdrawn payload", w[0].payload, "correction_id")
	codes := map[string]int{}
	for _, a := range auditFor(t, hid, "correction.decision_refused") {
		codes[a.payload["reason"].(string)+"/"+a.payload["attempted"].(string)]++
	}
	if codes["forbidden/withdraw"] != 1 || codes["already_decided/approve"] != 1 || codes["already_decided/reject"] != 2 || codes["already_decided/withdraw"] != 1 {
		t.Fatalf("refusals %v", codes)
	}
}

func TestApproveThenWithdraw409(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	if r := e.do(t, "POST", "/corrections/"+cid.String()+"/approve", mint(t, rev), "", nil); r.code != 204 {
		t.Fatalf("approve: %d %s", r.code, r.raw)
	}
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/withdraw", mint(t, sub), "", nil), 409, "already_decided")
	problem(t, e.do(t, "POST", "/corrections/"+cid.String()+"/approve", mint(t, clinician(t, hid)), "", nil), 409, "already_decided")
	if r := rowOf(t, cid); r.status != "approved" || *r.decidedBy != rev.id {
		t.Fatalf("row %+v", r)
	}
}

// Reject stores the code and note; the audit row carries the code only.
// The same Idempotency-Key replays the reply and decides once.
func TestRejectAndIdempotency(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)
	tok := mint(t, rev)
	for name, body := range map[string]string{
		"unknown code":  `{"reason_code":"bogus"}`,
		"no code":       `{}`,
		"note too long": `{"reason_code":"unsafe","note":"` + strings.Repeat("n", 501) + `"}`,
		"control char":  `{"reason_code":"unsafe","note":"Synthetic\u0007"}`,
		"extra field":   `{"reason_code":"unsafe","decided_by":"` + uuid.NewString() + `"}`,
		"no body":       ``,
	} {
		if r := e.do(t, "POST", "/corrections/"+cid.String()+"/reject", tok, body, nil); r.code != 422 {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
	if r := rowOf(t, cid); r.status != "pending_review" {
		t.Fatalf("a refused body changed the row: %+v", r)
	}
	key := map[string]string{"Idempotency-Key": "reject-" + uniq()}
	body := `{"reason_code":"needs_rewording","note":"  Synthetic note  "}`
	for i := 0; i < 2; i++ {
		if r := e.do(t, "POST", "/corrections/"+cid.String()+"/reject", tok, body, key); r.code != 204 {
			t.Fatalf("reject %d: %d %s", i, r.code, r.raw)
		}
	}
	var note *string
	if err := super.QueryRow(context.Background(), `SELECT rejection_note FROM feedback_events WHERE id = $1`, cid).Scan(&note); err != nil ||
		note == nil || *note != "Synthetic note" {
		t.Fatalf("note %v %v", note, err)
	}
	if r := rowOf(t, cid); r.status != "rejected" || r.code == nil || *r.code != "needs_rewording" || *r.decidedBy != rev.id {
		t.Fatalf("row %+v", r)
	}
	rej := auditFor(t, hid, "correction.rejected")
	if len(rej) != 1 {
		t.Fatalf("%d rejected rows (the replay decided twice?)", len(rej))
	}
	sameKeys(t, "correction.rejected payload", rej[0].payload, "correction_id", "reason_code")
	if rej[0].payload["reason_code"] != "needs_rewording" {
		t.Fatalf("payload %v", rej[0].payload)
	}
	// An empty note is no note.
	cid2 := submitCorrection(t, e, sub)
	if r := e.do(t, "POST", "/corrections/"+cid2.String()+"/reject", tok, `{"reason_code":"unsafe","note":"   "}`, nil); r.code != 204 {
		t.Fatalf("reject with a blank note: %d %s", r.code, r.raw)
	}
	if err := super.QueryRow(context.Background(), `SELECT rejection_note FROM feedback_events WHERE id = $1`, cid2).Scan(&note); err != nil || note != nil {
		t.Fatalf("blank note stored as %v %v", note, err)
	}
}

// --- reading ----------------------------------------------------------------

var (
	correctionKeys   = []string{"id", "query_id", "status", "corrected_diagnosis", "reason", "submitted_by", "submitted_at", "original", "consultation", "decided_by", "decided_at", "rejection"}
	personKeys       = []string{"display_name", "full_name", "specialty", "hospital"}
	originalKeys     = []string{"decision", "degraded", "diagnosis", "icd10_code", "response_text", "citations"}
	consultationKeys = []string{"age", "sex", "symptoms", "clinical_question", "urgency", "specialty"}
)

func checkCorrectionShape(t *testing.T, c map[string]any) {
	t.Helper()
	sameKeys(t, "CorrectionOut", c, correctionKeys...)
	if p, ok := c["submitted_by"].(map[string]any); ok {
		sameKeys(t, "submitted_by", p, personKeys...)
	} else {
		t.Errorf("submitted_by %v", c["submitted_by"])
	}
	if p, ok := c["decided_by"].(map[string]any); ok {
		sameKeys(t, "decided_by", p, personKeys...)
	}
	if o, ok := c["original"].(map[string]any); ok {
		sameKeys(t, "original", o, originalKeys...)
	} else {
		t.Errorf("original %v", c["original"])
	}
	if o, ok := c["consultation"].(map[string]any); ok {
		sameKeys(t, "consultation", o, consultationKeys...)
	} else {
		t.Errorf("consultation %v", c["consultation"])
	}
	if s, ok := c["submitted_at"].(string); !ok {
		t.Errorf("submitted_at %v", c["submitted_at"])
	} else if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("submitted_at %q", s)
	}
}

func find(list []any, id uuid.UUID) map[string]any {
	for _, x := range list {
		if m, ok := x.(map[string]any); ok && m["id"] == id.String() {
			return m
		}
	}
	return nil
}

// C14: the pending list shows the masked consultation only, never the
// stored question or patient data; and never the caller's own corrections.
func TestPendingListShowsMaskedTextOnly(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	cid := submitCorrection(t, e, sub)

	r := e.do(t, "GET", "/corrections?status=pending", mint(t, rev), "", nil)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if strings.Contains(r.raw, patientName) || strings.Contains(r.raw, patientMRN) {
		t.Fatalf("the stored patient details reached a reviewer: %s", r.raw)
	}
	c := find(r.list, cid)
	if c == nil {
		t.Fatalf("correction not listed: %s", r.raw)
	}
	checkCorrectionShape(t, c)
	cons := c["consultation"].(map[string]any)
	if cons["clinical_question"] != "Does [NAME] ([MRN]) need anticoagulation?" || cons["age"] != float64(61) || cons["sex"] != "male" ||
		cons["urgency"] != "urgent" || cons["specialty"] != "cardiology" {
		t.Errorf("consultation %v", cons)
	}
	if s, _ := cons["symptoms"].([]any); len(s) != 2 || s[1] != "[NAME]" {
		t.Errorf("symptoms %v", cons["symptoms"])
	}
	orig := c["original"].(map[string]any)
	if orig["decision"] != "PASS" || orig["degraded"] != false || orig["diagnosis"] != "Synthetic atrial fibrillation" ||
		orig["icd10_code"] != "I48.9" || orig["response_text"] != "Synthetic answer" {
		t.Errorf("original %v", orig)
	}
	if cites, ok := orig["citations"].([]any); !ok || cites == nil {
		t.Errorf("citations %v", orig["citations"])
	}
	p := c["submitted_by"].(map[string]any)
	if p["display_name"] != sub.name || p["full_name"] != "Synthetic Fullname" || p["specialty"] != "cardiology" || p["hospital"] != hospitalName {
		t.Errorf("submitted_by %v", p)
	}
	if strings.Contains(r.raw, sub.id.String()) || strings.Contains(r.raw, "synthetic.") {
		t.Errorf("a user id or email in the list: %s", r.raw)
	}
	if c["status"] != "pending_review" || c["decided_by"] != nil || c["decided_at"] != nil || c["rejection"] != nil {
		t.Errorf("pending row %v", c)
	}
	// The submitter's own waiting correction is not in their queue.
	if mine := e.do(t, "GET", "/corrections?status=pending", mint(t, sub), "", nil); mine.code != 200 || find(mine.list, cid) != nil {
		t.Fatalf("own correction in the queue: %d %s", mine.code, mine.raw)
	}
	// No status means pending; any other status is 422.
	if all := e.do(t, "GET", "/corrections", mint(t, rev), "", nil); all.code != 200 || find(all.list, cid) == nil {
		t.Fatalf("GET /corrections: %d %s", all.code, all.raw)
	}
	for _, q := range []string{"?status=approved", "?status=", "?status=pending&status=pending"} {
		if bad := e.do(t, "GET", "/corrections"+q, mint(t, rev), "", nil); bad.code != 422 {
			t.Errorf("%s: %d", q, bad.code)
		}
	}
}

// Mine shows every status with the rejection note to its submitter;
// reviewed shows the reviewer's decisions.
func TestMineAndReviewed(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	rejected := submitCorrection(t, e, sub)
	approved := submitCorrection(t, e, sub)
	waiting := submitCorrection(t, e, sub)
	rtok := mint(t, rev)
	if r := e.do(t, "POST", "/corrections/"+rejected.String()+"/reject", rtok, `{"reason_code":"unsafe","note":"Synthetic note"}`, nil); r.code != 204 {
		t.Fatal(r.raw)
	}
	if r := e.do(t, "POST", "/corrections/"+approved.String()+"/approve", rtok, "", nil); r.code != 204 {
		t.Fatal(r.raw)
	}
	mine := e.do(t, "GET", "/corrections/mine", mint(t, sub), "", nil)
	if mine.code != 200 || len(mine.list) != 3 {
		t.Fatalf("mine: %d %s", mine.code, mine.raw)
	}
	// Newest first.
	if mine.list[0].(map[string]any)["id"] != waiting.String() {
		t.Errorf("mine not newest first: %s", mine.raw)
	}
	r := find(mine.list, rejected)
	checkCorrectionShape(t, r)
	rej, _ := r["rejection"].(map[string]any)
	if r["status"] != "rejected" || rej["reason_code"] != "unsafe" || rej["note"] != "Synthetic note" || r["decided_at"] == nil {
		t.Errorf("rejected in mine: %v", r)
	}
	if d, _ := r["decided_by"].(map[string]any); d["display_name"] != rev.name {
		t.Errorf("decided_by %v", r["decided_by"])
	}
	reviewed := e.do(t, "GET", "/corrections/reviewed", rtok, "", nil)
	if reviewed.code != 200 || len(reviewed.list) != 2 || find(reviewed.list, waiting) != nil {
		t.Fatalf("reviewed: %d %s", reviewed.code, reviewed.raw)
	}
	for _, x := range reviewed.list {
		checkCorrectionShape(t, x.(map[string]any))
	}
	if strings.Contains(reviewed.raw, patientName) || strings.Contains(mine.raw, patientMRN) {
		t.Fatal("stored patient details in a list")
	}
	// Decisions older than 30 days drop off.
	if _, err := super.Exec(context.Background(), `ALTER TABLE feedback_events DISABLE TRIGGER feedback_transition_check`); err != nil {
		t.Fatal(err)
	}
	_, err := super.Exec(context.Background(), `UPDATE feedback_events SET decided_at = now() - interval '31 days' WHERE id = $1`, rejected)
	if _, err2 := super.Exec(context.Background(), `ALTER TABLE feedback_events ENABLE TRIGGER feedback_transition_check`); err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if reviewed = e.do(t, "GET", "/corrections/reviewed", rtok, "", nil); len(reviewed.list) != 1 || find(reviewed.list, approved) == nil {
		t.Fatalf("reviewed after 31 days: %s", reviewed.raw)
	}
}

func TestStatsMine(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	tok := mint(t, sub)
	if r := e.do(t, "GET", "/feedback/stats/mine", tok, "", nil); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw)
	} else {
		sameKeys(t, "FeedbackStatsOut", r.body, "agreed", "corrections_waiting", "corrections_approved", "corrections_rejected", "corrections_withdrawn")
		for k, v := range r.body {
			if v != float64(0) {
				t.Errorf("empty stats %s = %v", k, v)
			}
		}
	}
	for i := 0; i < 2; i++ {
		if r := e.do(t, "POST", "/feedback", tok, validateBody(seedQuery(t, sub, true)), nil); r.code != 200 {
			t.Fatal(r.raw)
		}
	}
	submitCorrection(t, e, sub)
	rtok := mint(t, rev)
	a := submitCorrection(t, e, sub)
	e.do(t, "POST", "/corrections/"+a.String()+"/approve", rtok, "", nil)
	rj := submitCorrection(t, e, sub)
	e.do(t, "POST", "/corrections/"+rj.String()+"/reject", rtok, `{"reason_code":"out_of_scope"}`, nil)
	w := submitCorrection(t, e, sub)
	e.do(t, "POST", "/corrections/"+w.String()+"/withdraw", tok, "", nil)
	// Someone else's feedback doesn't count.
	submitCorrection(t, e, rev)

	r := e.do(t, "GET", "/feedback/stats/mine", tok, "", nil)
	want := map[string]float64{"agreed": 2, "corrections_waiting": 1, "corrections_approved": 1, "corrections_rejected": 1, "corrections_withdrawn": 1}
	for k, v := range want {
		if r.body[k] != v {
			t.Errorf("%s = %v, want %v (%s)", k, r.body[k], v, r.raw)
		}
	}
}

// C16, core part: the free text (reason, corrected diagnosis, rejection
// note) reaches no log line, span or audit payload, on success or refusal.
func TestFeedbackTextNeverLeaves(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := telemetry.SetupWith(exp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	e := newEnv(t)
	hid := seedHospital(t)
	sub := clinician(t, hid)
	rev := clinician(t, hid)
	tok, rtok := mint(t, sub), mint(t, rev)

	// Refused submissions carrying the marker.
	q := seedQuery(t, sub, true)
	e.do(t, "POST", "/feedback", tok, correctBody(q, marker+"\x00", marker), nil)
	e.do(t, "POST", "/feedback", tok, correctBody(q, strings.Repeat(marker, 20), marker), nil)
	e.do(t, "POST", "/feedback", tok, correctBody(seedQuery(t, sub, false), marker, marker), nil)
	// Accepted ones.
	r := e.do(t, "POST", "/feedback", tok, correctBody(q, "Dx "+marker, "Reason "+marker), map[string]string{"Idempotency-Key": "t-" + uniq()})
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	cid := r.body["correction_id"].(string)
	e.do(t, "POST", "/corrections/"+cid+"/approve", tok, "", nil) // self_approval
	e.do(t, "POST", "/corrections/"+cid+"/reject", rtok, `{"reason_code":"bogus","note":"`+marker+`"}`, nil)
	if r := e.do(t, "POST", "/corrections/"+cid+"/reject", rtok, `{"reason_code":"unsafe","note":"Note `+marker+`"}`, nil); r.code != 204 {
		t.Fatalf("reject: %d %s", r.code, r.raw)
	}
	e.do(t, "POST", "/corrections/"+cid+"/withdraw", tok, "", nil) // already_decided
	e.do(t, "GET", "/corrections/mine", tok, "", nil)
	e.do(t, "GET", "/corrections/reviewed", rtok, "", nil)

	for _, s := range exp.GetSpans() {
		texts := []string{s.Name, s.Status.Description}
		for _, kv := range s.Attributes {
			texts = append(texts, string(kv.Key), kv.Value.Emit())
		}
		for _, ev := range s.Events {
			texts = append(texts, ev.Name)
			for _, kv := range ev.Attributes {
				texts = append(texts, kv.Value.Emit())
			}
		}
		for _, x := range texts {
			if strings.Contains(x, marker) {
				t.Fatalf("the marker reached span %q", s.Name)
			}
		}
	}
	if len(exp.GetSpans()) == 0 {
		t.Fatal("no spans: the test proves nothing")
	}
	if strings.Contains(e.logs.String(), marker) {
		t.Fatal("the marker reached the logs")
	}
	if e.logs.String() == "" {
		t.Fatal("nothing logged: the test proves nothing")
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE payload::text LIKE '%' || $1 || '%'`, marker).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d audit payloads hold the marker (%v)", n, err)
	}
	if rows := auditFor(t, hid, ""); len(rows) < 4 {
		t.Fatalf("only %d audit rows: the test proves nothing", len(rows))
	}
}
