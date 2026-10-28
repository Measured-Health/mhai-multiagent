//go:build integration

package grant_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/grant"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/natsapi"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const (
	testMFAKey   = "test-only-mfa-key-0123456789abcdef0123456789"
	testPassword = "amber-quay-signal-4182" // synthetic
	bffPrefix    = "_INBOX_bff.clinical."
	pingSubject  = "inference.v1.echo.ping"
)

var (
	super, app *pgxpool.Pool
	natsSrv    *testutil.NATS
	buckets    bus.Buckets
	issuer     token.Issuer
	signer     reqtoken.Signer
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
		if buckets, err = bus.OpenBuckets(ctx, js, 1); err != nil {
			fmt.Fprintln(os.Stderr, "buckets:", err)
			return 1
		}
		if err := bus.EnsureStreams(ctx, js, bus.StreamConfig{Replicas: 1, PipelineMaxAckPending: 8}); err != nil {
			fmt.Fprintln(os.Stderr, "streams:", err)
			return 1
		}
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		issuer = token.Issuer{Key: priv, KeyID: "test-access-kid"}
		_, rpriv, _ := ed25519.GenerateKey(rand.Reader)
		signer = reqtoken.Signer{Key: rpriv, KeyID: reqtoken.KeyID(rpriv.Public().(ed25519.PublicKey))}
		if pwHash, err = password.Hash(testPassword); err != nil {
			fmt.Fprintln(os.Stderr, "hash:", err)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

// --- Harness -----------------------------------------------------------------

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

func connect(t *testing.T, url, inbox string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(url, nats.CustomInboxPrefix(inbox), nats.MaxReconnects(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func newGuard(log *syncBuffer) *authz.Guard {
	l := logx.New(log)
	return &authz.Guard{Keys: issuer.PublicKeys(), Denylist: buckets.Denylist, Pool: app, Log: l,
		Idem: httpx.IdempotentLog(app, l), IdemAnon: httpx.IdempotentAnonLog(app, l)}
}

// coreCopy is one copy of core's grant endpoint on its own gateway
// connection.
type coreCopy struct {
	nc      *nats.Conn
	stop    func()
	metrics *grant.Metrics
	logs    *syncBuffer
}

type copyOpts struct {
	guard      *authz.Guard
	limits     grant.Limits
	noTestOnly bool
	logs       *syncBuffer
	pool       *pgxpool.Pool // the server's audit pool; default app
	summary    time.Duration
}

func startCore(t *testing.T, o copyOpts) *coreCopy {
	t.Helper()
	if o.logs == nil {
		o.logs = &syncBuffer{}
	}
	if o.guard == nil {
		o.guard = newGuard(o.logs)
	}
	c := &coreCopy{nc: connect(t, natsSrv.GatewayURL, bus.GatewayInboxPrefix), metrics: &grant.Metrics{}, logs: o.logs}
	if o.pool == nil {
		o.pool = app
	}
	s := &grant.Server{Guard: o.guard, Signer: signer, Pool: o.pool, Log: logx.New(o.logs), Limits: o.limits,
		Metrics: c.metrics, AllowTestOnly: !o.noTestOnly, SummaryEvery: o.summary}
	stop, err := s.Serve(context.Background(), c.nc)
	if err != nil {
		t.Fatal(err)
	}
	c.stop = stop
	t.Cleanup(stop)
	return c
}

func echoConn(t *testing.T) *nats.Conn {
	return connect(t, natsSrv.EchoURL, bus.WorkerInbox("echo"))
}

func pipelineConn(t *testing.T) *nats.Conn {
	return connect(t, natsSrv.PipelineURL, bus.WorkerInbox("pipeline"))
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func newRID() string { return "rid-" + strings.ReplaceAll(uuid.NewString(), "-", "") }

// pingRequest is a well-formed grant request for echo.ping with body.
func pingRequest(tok string, body []byte) *intelpb.GrantRequest {
	return &intelpb.GrantRequest{AccessToken: tok, Operation: "echo.ping", Subject: pingSubject,
		BodyHash: bodyHash(body), ReplyPrefix: bffPrefix, RequestId: newRID(), DeadlineMs: 3000}
}

// answer is a decoded grant reply.
type answer struct {
	token string
	code  string // "" when granted
}

func ask(t *testing.T, nc *nats.Conn, service string, req *intelpb.GrantRequest) (answer, error) {
	t.Helper()
	data, err := proto.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := nc.Request(grant.SubjectPrefix+service, data, 5*time.Second)
	if err != nil {
		return answer{}, err
	}
	return decode(t, m), nil
}

func decode(t *testing.T, m *nats.Msg) answer {
	t.Helper()
	status, schema := m.Header.Get(grant.HeaderStatus), m.Header.Get(grant.HeaderSchema)
	if status == grant.StatusOK {
		if schema != grant.SchemaResponse {
			t.Fatalf("granted with schema %q", schema)
		}
		var r intelpb.GrantResponse
		if err := proto.Unmarshal(m.Data, &r); err != nil || r.RequestToken == "" {
			t.Fatalf("grant response: %v", err)
		}
		return answer{token: r.RequestToken}
	}
	if schema != grant.SchemaRefused {
		t.Fatalf("refused with schema %q (status %q)", schema, status)
	}
	var r intelpb.GrantRefused
	if err := proto.Unmarshal(m.Data, &r); err != nil || r.Code == "" || r.Code != status {
		t.Fatalf("grant refused: %v code %q status %q", err, r.Code, status)
	}
	return answer{code: r.Code}
}

func mustAsk(t *testing.T, nc *nats.Conn, service string, req *intelpb.GrantRequest) answer {
	t.Helper()
	a, err := ask(t, nc, service, req)
	if err != nil {
		t.Fatalf("grant request: %v", err)
	}
	return a
}

// --- Seeding (synthetic data only) -------------------------------------------

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
	hid   uuid.UUID
	roles []string
	mfa   bool
}

// seedUser makes an active account; mfa says whether two-step is on.
func seedUser(t *testing.T, hospital uuid.UUID, mfa bool, roles ...string) user {
	t.Helper()
	ctx := context.Background()
	var hid any
	if hospital != uuid.Nil {
		hid = hospital
	}
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, full_name, role_title, is_active)
		VALUES ($1, $2, $3, 'Dr Synthetic', 'Synthetic Person', 'Registrar', true) RETURNING id`,
		hid, "synthetic.grant."+uniq()+"@havana.example", pwHash).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := super.Exec(ctx, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r); err != nil {
			t.Fatal(err)
		}
	}
	if mfa {
		if _, err := super.Exec(ctx, `INSERT INTO user_mfa (user_id, secret_enc, enabled, backup_code_hashes)
			VALUES ($1, '\x00', true, ARRAY['a','b','c'])`, id); err != nil {
			t.Fatal(err)
		}
	}
	return user{id: id, hid: hospital, roles: roles, mfa: mfa}
}

// session is a signed-in session and its access token.
type session struct {
	sid uuid.UUID
	tok string
}

// signIn opens a session for u (proven by a second factor when u has
// two-step on) and returns it with an access token from iss.
func signIn(t *testing.T, u user, iss token.Issuer) session {
	t.Helper()
	sid := uuid.New()
	amr := []string{"pwd"}
	if u.mfa {
		amr = append(amr, "otp")
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, $3, $4)`,
		sid, u.id, u.mfa, amr); err != nil {
		t.Fatal(err)
	}
	hid := ""
	if u.hid != uuid.Nil {
		hid = u.hid.String()
	}
	at, _, err := iss.Access(token.Claims{Sub: u.id.String(), Hid: hid, Roles: u.roles, MFA: u.mfa, AMR: amr,
		Sid: sid.String(), Jti: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return session{sid: sid, tok: at}
}

func clinician(t *testing.T) (user, session) {
	t.Helper()
	u := seedUser(t, seedHospital(t), true, authz.RoleClinician)
	return u, signIn(t, u, issuer)
}

type auditRow struct {
	actor, hospital, requestID, entityID *string
	payload                              map[string]any
	raw                                  string
}

func auditRows(t *testing.T, action string, actor uuid.UUID) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT actor_id::text, hospital_id::text, request_id, entity_id, payload::text
		FROM audit_log WHERE action = $1 AND actor_id = $2 ORDER BY id`, action, actor)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actor, &r.hospital, &r.requestID, &r.entityID, &r.raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal([]byte(r.raw), &r.payload)
		out = append(out, r)
	}
	return out
}

func countAction(t *testing.T, action, reason string) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = $1 AND payload->>'reason' = $2`,
		action, reason).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func allAuditText(t *testing.T) string {
	t.Helper()
	var s string
	if err := super.QueryRow(context.Background(), `SELECT COALESCE(string_agg(payload::text || coalesce(request_id, '') || coalesce(entity_id, ''), ' '), '') FROM audit_log`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// --- Tests -------------------------------------------------------------------

// TestGrantBindsTheCall (A29, core side): the token core returns verifies
// only for that subject, body, audience echo and reply prefix
// _INBOX_bff.clinical., carries the principal and the BFF's request id,
// and lives no longer than the smaller of the operation's timeout and the
// BFF's deadline.
func TestGrantBindsTheCall(t *testing.T) {
	startCore(t, copyOpts{})
	u, s := clinician(t)
	body := []byte("synthetic ping body")
	req := pingRequest(s.tok, body)
	a := mustAsk(t, echoConn(t), "echo", req)
	if a.code != "" {
		t.Fatalf("refused: %s", a.code)
	}
	now := time.Now()
	keys := signer.PublicKeys()
	want := reqtoken.Expect{Aud: "echo", Subject: pingSubject, Kind: reqtoken.KindReq}
	c, err := reqtoken.VerifyAt(a.token, keys, want, body, now)
	if err != nil {
		t.Fatalf("granted token doesn't verify: %v", err)
	}
	if c.Sub != u.id.String() || c.Hid != u.hid.String() || len(c.Roles) != 1 || c.Roles[0] != authz.RoleClinician ||
		c.Rid != req.RequestId || c.Via != reqtoken.ViaGrant || c.ReplyPrefix != bffPrefix || c.Kind != reqtoken.KindReq {
		t.Fatalf("claims: %+v", c)
	}
	if ttl := c.ExpiresAt.Sub(c.IssuedAt); ttl != 3*time.Second {
		t.Fatalf("lifetime %v, want the BFF's 3 s deadline", ttl)
	}
	if err := reqtoken.CheckReply(c, bffPrefix+"abc.1", true); err != nil {
		t.Fatalf("the BFF's own inbox refused: %v", err)
	}
	// Useless for anything else.
	if _, err := reqtoken.VerifyAt(a.token, keys, want, []byte("another body"), now); !errors.Is(err, reqtoken.ErrBinding) {
		t.Errorf("another body: %v", err)
	}
	if _, err := reqtoken.VerifyAt(a.token, keys, reqtoken.Expect{Aud: "echo", Subject: "inference.v1.echo.other", Kind: reqtoken.KindReq}, body, now); !errors.Is(err, reqtoken.ErrBinding) {
		t.Errorf("another subject: %v", err)
	}
	if _, err := reqtoken.VerifyAt(a.token, keys, reqtoken.Expect{Aud: "pipeline", Subject: pingSubject, Kind: reqtoken.KindReq}, body, now); !errors.Is(err, reqtoken.ErrClaims) {
		t.Errorf("another service: %v", err)
	}
	for _, reply := range []string{"_INBOX_bff.platform.abc", "_INBOX_gateway.abc", "_INBOX_worker.echo.abc", bffPrefix} {
		if err := reqtoken.CheckReply(c, reply, true); err == nil {
			t.Errorf("reply %q accepted", reply)
		}
	}
	if err := reqtoken.CheckReply(c, bffPrefix+"abc", false); err == nil {
		t.Error("a granted token passed as an in-message (core) token")
	}

	// The operation's own timeout caps a longer BFF deadline.
	req = pingRequest(s.tok, body)
	req.DeadlineMs = 50_000
	a = mustAsk(t, echoConn(t), "echo", req)
	if a.code != "" {
		t.Fatalf("refused: %s", a.code)
	}
	c, err = reqtoken.VerifyAt(a.token, keys, want, body, time.Now())
	if err != nil || c.ExpiresAt.Sub(c.IssuedAt) != 5*time.Second {
		t.Fatalf("lifetime %v (%v), want echo.ping's 5 s", c.ExpiresAt.Sub(c.IssuedAt), err)
	}
}

// TestGrantRefusesOtherServicesOperation (A30): a worker can't get a grant
// for another service's operation, nor for a job.
func TestGrantRefusesOtherServicesOperation(t *testing.T) {
	startCore(t, copyOpts{})
	_, s := clinician(t)
	pc := pipelineConn(t)
	body := []byte("synthetic")
	if a := mustAsk(t, pc, "pipeline", pingRequest(s.tok, body)); a.code != "forbidden" {
		t.Errorf("pipeline asking for echo.ping: %+v", a)
	}
	req := pingRequest(s.tok, body)
	req.Operation, req.Subject = "pipeline.run_query", "inference.v1.pipeline.run_query"
	if a := mustAsk(t, pc, "pipeline", req); a.code != "forbidden" {
		t.Errorf("pipeline.run_query (a job): %+v", a)
	}
	req.Operation, req.Subject = "pipeline.health", "inference.v1.pipeline.health"
	if a := mustAsk(t, pc, "pipeline", req); a.code != "forbidden" {
		t.Errorf("pipeline.health (not bff_direct): %+v", a)
	}
}

// TestGrantRefusesBadRequests: each binding field core checks.
func TestGrantRefusesBadRequests(t *testing.T) {
	startCore(t, copyOpts{})
	u, s := clinician(t)
	ec := echoConn(t)
	body := []byte("synthetic")
	cases := []struct {
		name string
		edit func(*intelpb.GrantRequest)
		want string
	}{
		{"subject of another op", func(r *intelpb.GrantRequest) { r.Subject = "inference.v1.echo.other" }, "forbidden"},
		{"subject of another service", func(r *intelpb.GrantRequest) { r.Subject = "inference.v1.pipeline.ping" }, "forbidden"},
		{"unknown operation", func(r *intelpb.GrantRequest) { r.Operation = "echo.nope" }, "forbidden"},
		{"platform reply prefix", func(r *intelpb.GrantRequest) { r.ReplyPrefix = "_INBOX_bff.platform." }, "forbidden"},
		{"no reply prefix", func(r *intelpb.GrantRequest) { r.ReplyPrefix = "" }, "forbidden"},
		{"short body hash", func(r *intelpb.GrantRequest) { r.BodyHash = r.BodyHash[:42] }, "forbidden"},
		{"padded body hash", func(r *intelpb.GrantRequest) { r.BodyHash = r.BodyHash[:42] + "=" }, "forbidden"},
		{"patient ids", func(r *intelpb.GrantRequest) { r.PatientIds = []string{uuid.NewString()} }, "not_found"},
		{"short request id", func(r *intelpb.GrantRequest) { r.RequestId = "abc" }, "forbidden"},
		{"request id with a dot", func(r *intelpb.GrantRequest) { r.RequestId = "abcdefgh.ijk" }, "forbidden"},
		{"long request id", func(r *intelpb.GrantRequest) { r.RequestId = strings.Repeat("a", 65) }, "forbidden"},
		{"no deadline", func(r *intelpb.GrantRequest) { r.DeadlineMs = 0 }, "forbidden"},
		{"no access token", func(r *intelpb.GrantRequest) { r.AccessToken = "" }, "unauthenticated"},
		{"forged access token", func(r *intelpb.GrantRequest) { r.AccessToken = s.tok[:len(s.tok)-4] + "AAAA" }, "unauthenticated"},
	}
	for _, c := range cases {
		req := pingRequest(s.tok, body)
		c.edit(req)
		if a := mustAsk(t, ec, "echo", req); a.code != c.want {
			t.Errorf("%s: %+v, want %s", c.name, a, c.want)
		}
	}
	// Garbage in the body: refused, not dropped.
	m, err := ec.Request(grant.SubjectPrefix+"echo", []byte{0xff, 0xff, 0xff}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if a := decode(t, m); a.code != "forbidden" {
		t.Errorf("garbage body: %+v", a)
	}
	// Refusals after the principal is known are audited under it.
	rows := auditRows(t, "intelligence.grant_refused", u.id)
	if len(rows) < 12 {
		t.Fatalf("%d refusal rows for the user", len(rows))
	}
	if a := mustAsk(t, ec, "echo", pingRequest(s.tok, body)); a.code != "" {
		t.Fatalf("a good request after the refusals: %s", a.code)
	}
}

// TestGrantRefusesTestOnlyOutsideDevelopment: echo.ping is test-only;
// a core that doesn't allow test-only operations refuses its grant.
func TestGrantRefusesTestOnlyOutsideDevelopment(t *testing.T) {
	startCore(t, copyOpts{noTestOnly: true})
	_, s := clinician(t)
	if a := mustAsk(t, echoConn(t), "echo", pingRequest(s.tok, []byte("x"))); a.code != "forbidden" {
		t.Fatalf("test-only operation granted: %+v", a)
	}
}

// TestGrantRateLimited (A31): 200 parallel grants for one session get at
// most the per-session limit; every other reply is rate_limited, at once.
// Meanwhile an api.v1.clinical call from another hospital is unaffected.
func TestGrantRateLimited(t *testing.T) {
	var frozen atomic.Int64
	frozen.Store(time.Now().UnixNano())
	clock := func() time.Time { return time.Unix(0, frozen.Load()) }
	startCore(t, copyOpts{limits: grant.Limits{InFlight: 512, Now: clock}})
	_, s := clinician(t)

	// Core's api.v1 for another hospital's user.
	logs := &syncBuffer{}
	l := logx.New(logs)
	guard := newGuard(logs)
	m := authz.NewMux(guard)
	server.Register(m, server.Deps{Pool: app, Auth: &auth.Service{Pool: app, Buckets: buckets, Issuer: issuer,
		Keys: issuer.PublicKeys(), Log: l, MFAKey: []byte(testMFAKey), Argon: password.NewGate(4, time.Minute)}})
	stopAPI, err := natsapi.Serve(context.Background(), connect(t, natsSrv.GatewayURL, bus.GatewayInboxPrefix), m.NATSHandler(), l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stopAPI)
	bff := connect(t, natsSrv.BFFClinicalURL, bus.BFFInbox("clinical"))
	other := seedUser(t, seedHospital(t), true, authz.RoleClinician)
	otherTok := signIn(t, other, issuer).tok
	apiCall := func() (int, time.Duration) {
		msg := nats.NewMsg(natsapi.SubjectClinical)
		msg.Header.Set(natsapi.HeaderMethod, http.MethodGet)
		msg.Header.Set(natsapi.HeaderPath, httpx.BasePath+"/auth/sessions")
		msg.Header.Set("Authorization", "Bearer "+otherTok)
		msg.Header.Set("X-MHAI-Client-IP", "198.51.100.9")
		start := time.Now()
		r, err := bff.RequestMsg(msg, 5*time.Second)
		if err != nil {
			return 0, time.Since(start)
		}
		code, _ := strconv.Atoi(r.Header.Get(natsapi.HeaderStatus))
		return code, time.Since(start)
	}
	if code, _ := apiCall(); code != 200 { // warm the account cache
		t.Fatalf("api.v1 warm-up: %d", code)
	}

	ec := echoConn(t)
	body := []byte("synthetic")
	type result struct {
		a       answer
		elapsed time.Duration
		err     error
	}
	results := make([]result, 200)
	var wg sync.WaitGroup
	var apiCode int
	var apiTook time.Duration
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data, _ := proto.Marshal(pingRequest(s.tok, body))
			start := time.Now()
			msg, err := ec.Request(grant.SubjectPrefix+"echo", data, 10*time.Second)
			r := result{elapsed: time.Since(start), err: err}
			if err == nil {
				r.a = decode(t, msg)
			}
			results[i] = r
		}(i)
		if i == 100 {
			wg.Add(1)
			go func() { defer wg.Done(); apiCode, apiTook = apiCall() }()
		}
	}
	wg.Wait()
	granted, limited := 0, 0
	var slowest time.Duration
	for _, r := range results {
		switch {
		case r.err != nil:
			t.Errorf("no reply: %v", r.err)
		case r.a.code == "":
			granted++
		case r.a.code == "rate_limited":
			limited++
			slowest = max(slowest, r.elapsed)
		default:
			t.Errorf("unexpected refusal %q", r.a.code)
		}
	}
	if granted == 0 || granted > grant.DefaultPerSession {
		t.Errorf("granted %d, want 1..%d", granted, grant.DefaultPerSession)
	}
	if granted+limited != len(results) {
		t.Errorf("granted %d + limited %d != %d", granted, limited, len(results))
	}
	t.Logf("granted %d, rate_limited %d, slowest rate_limited reply %v; api.v1 %d in %v", granted, limited, slowest, apiCode, apiTook)
	if slowest > 50*time.Millisecond {
		t.Errorf("slowest rate_limited reply took %v, want within 50 ms", slowest)
	}
	if apiCode != 200 || apiTook > 200*time.Millisecond {
		t.Errorf("api.v1 for another hospital: %d in %v, want 200 within 200 ms", apiCode, apiTook)
	}
	// Rate-limit refusals are metrics only, never on the chain.
	if n := countAction(t, "intelligence.grant_refused", "rate_limited"); n != 0 {
		t.Errorf("%d rate_limited refusals audited", n)
	}
	// A second later, the session may ask again.
	frozen.Add(int64(time.Second))
	if a := mustAsk(t, ec, "echo", pingRequest(s.tok, body)); a.code != "" {
		t.Errorf("after a second: %+v", a)
	}
}

// TestGrantPerHospitalLimit: many sessions of one hospital share its rate.
func TestGrantPerHospitalLimit(t *testing.T) {
	var frozen atomic.Int64
	frozen.Store(time.Now().UnixNano())
	startCore(t, copyOpts{limits: grant.Limits{PerSession: 100, PerHospital: 5, InFlight: 64,
		Now: func() time.Time { return time.Unix(0, frozen.Load()) }}})
	hid := seedHospital(t)
	ec := echoConn(t)
	granted, limited := 0, 0
	for i := 0; i < 4; i++ {
		u := seedUser(t, hid, true, authz.RoleClinician)
		s := signIn(t, u, issuer)
		for j := 0; j < 3; j++ {
			switch a := mustAsk(t, ec, "echo", pingRequest(s.tok, []byte("x"))); a.code {
			case "":
				granted++
			case "rate_limited":
				limited++
			default:
				t.Fatalf("refused %s", a.code)
			}
		}
	}
	if granted != 5 || limited != 7 {
		t.Fatalf("granted %d, limited %d; want 5 and 7", granted, limited)
	}
}

// TestGrantRateLimitNotKeyedOnUnverifiedClaims (F8): forged tokens naming
// a victim's session don't drain the victim's bucket.
func TestGrantRateLimitNotKeyedOnUnverifiedClaims(t *testing.T) {
	var frozen atomic.Int64
	frozen.Store(time.Now().UnixNano())
	startCore(t, copyOpts{limits: grant.Limits{Now: func() time.Time { return time.Unix(0, frozen.Load()) }}})
	u, s := clinician(t)
	_, otherKey, _ := ed25519.GenerateKey(rand.Reader)
	forger := token.Issuer{Key: otherKey, KeyID: issuer.KeyID}
	forged, _, err := forger.Access(token.Claims{Sub: u.id.String(), Hid: u.hid.String(), Roles: u.roles, MFA: true,
		AMR: []string{"pwd", "otp"}, Sid: s.sid.String(), Jti: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	ec := echoConn(t)
	for i := 0; i < 3*grant.DefaultPerSession; i++ {
		if a := mustAsk(t, ec, "echo", pingRequest(forged, []byte("x"))); a.code != "unauthenticated" {
			t.Fatalf("forged token: %+v", a)
		}
	}
	if a := mustAsk(t, ec, "echo", pingRequest(s.tok, []byte("x"))); a.code != "" {
		t.Fatalf("the victim was limited by forged tokens: %+v", a)
	}
}

// failingKV is a denylist that can't be read, or blocks until released.
type failingKV struct {
	jetstream.KeyValue
	block chan struct{} // nil: fail at once
}

func (f failingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
		return f.KeyValue.Get(ctx, key)
	}
	return nil, errors.New("synthetic outage")
}

// TestGrantNotReady (F21): the denylist down → GrantRefused{not_ready}.
func TestGrantNotReady(t *testing.T) {
	logs := &syncBuffer{}
	g := newGuard(logs)
	g.Denylist = failingKV{KeyValue: buckets.Denylist}
	startCore(t, copyOpts{guard: g, logs: logs})
	_, s := clinician(t)
	if a := mustAsk(t, echoConn(t), "echo", pingRequest(s.tok, []byte("x"))); a.code != "not_ready" {
		t.Fatalf("denylist down: %+v", a)
	}
}

// TestGrantOverloaded: past the in-flight bound, an answer at once, with
// no token work and no audit row.
func TestGrantOverloaded(t *testing.T) {
	logs := &syncBuffer{}
	g := newGuard(logs)
	release := make(chan struct{})
	g.Denylist = failingKV{KeyValue: buckets.Denylist, block: release}
	c := startCore(t, copyOpts{guard: g, logs: logs, limits: grant.Limits{InFlight: 1}})
	u, s := clinician(t)
	ec := echoConn(t)
	held := make(chan answer, 1)
	go func() {
		a, _ := ask(t, ec, "echo", pingRequest(s.tok, []byte("x")))
		held <- a
	}()
	deadline := time.Now().Add(5 * time.Second)
	for c.metrics.InFlight.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first grant never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	a := mustAsk(t, ec, "echo", pingRequest(s.tok, []byte("x")))
	if a.code != grant.CodeOverloaded || time.Since(start) > time.Second {
		t.Fatalf("overflow: %+v after %v", a, time.Since(start))
	}
	if c.metrics.Overloaded.Load() != 1 {
		t.Fatalf("overloaded metric %d", c.metrics.Overloaded.Load())
	}
	close(release)
	if a := <-held; a.code != "" {
		t.Fatalf("held grant: %+v", a)
	}
	if n := countAction(t, "intelligence.grant_refused", grant.CodeOverloaded); n != 0 {
		t.Errorf("%d overload refusals audited", n)
	}
	if rows := auditRows(t, "intelligence.grant_refused", u.id); len(rows) != 0 {
		t.Errorf("refusal audited: %+v", rows)
	}
}

// TestGrantRefusesBadSessions (A35): a logged-out session, a deactivated
// user (past the 30 s cache), a hospital admin without two-step and an
// expired token are each refused with core's own code and audited with
// no token in the row.
func TestGrantRefusesBadSessions(t *testing.T) {
	var cacheClock atomic.Int64
	cacheClock.Store(time.Now().UnixNano())
	logs := &syncBuffer{}
	g := newGuard(logs)
	g.CacheNow = func() time.Time { return time.Unix(0, cacheClock.Load()) }
	startCore(t, copyOpts{guard: g, logs: logs})
	ec := echoConn(t)
	body := []byte("x")
	ctx := context.Background()

	// Logged out: the session is on the denylist.
	u1, s1 := clinician(t)
	if _, err := buckets.Denylist.Put(ctx, authz.SIDKey(s1.sid.String()), []byte("1")); err != nil {
		t.Fatal(err)
	}
	if a := mustAsk(t, ec, "echo", pingRequest(s1.tok, body)); a.code != "unauthenticated" {
		t.Errorf("logged-out session: %+v", a)
	}

	// Deactivated, once the cached state is older than 30 s.
	u2, s2 := clinician(t)
	if a := mustAsk(t, ec, "echo", pingRequest(s2.tok, body)); a.code != "" {
		t.Fatalf("active user refused: %s", a.code)
	}
	if _, err := super.Exec(ctx, `UPDATE users SET is_active = false WHERE id = $1`, u2.id); err != nil {
		t.Fatal(err)
	}
	cacheClock.Add(int64(authz.MaxCacheTTL + time.Second))
	if a := mustAsk(t, ec, "echo", pingRequest(s2.tok, body)); a.code != "unauthenticated" {
		t.Errorf("deactivated user: %+v", a)
	}

	// A hospital admin (and clinician) without two-step.
	u3 := seedUser(t, seedHospital(t), false, authz.RoleClinician, authz.RoleHospitalAdmin)
	s3 := signIn(t, u3, issuer)
	if a := mustAsk(t, ec, "echo", pingRequest(s3.tok, body)); a.code != "mfa_required" {
		t.Errorf("admin without two-step: %+v", a)
	}

	// Revoked in the database (not on the denylist).
	u5, s5 := clinician(t)
	if _, err := super.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1`, s5.sid); err != nil {
		t.Fatal(err)
	}
	if a := mustAsk(t, ec, "echo", pingRequest(s5.tok, body)); a.code != "unauthenticated" {
		t.Errorf("revoked session: %+v", a)
	}

	// A platform account: no clinical principal on the clinical audience.
	u6 := seedUser(t, uuid.Nil, true, authz.RolePlatformAdmin)
	s6 := signIn(t, u6, issuer)
	if a := mustAsk(t, ec, "echo", pingRequest(s6.tok, body)); a.code != "forbidden" {
		t.Errorf("platform account: %+v", a)
	}
	if rows := auditRows(t, "intelligence.grant_refused", u6.id); len(rows) != 1 || rows[0].payload["reason"] != "forbidden" || rows[0].hospital != nil {
		t.Errorf("platform refusal rows: %+v", rows)
	}

	// Expired.
	u4 := seedUser(t, seedHospital(t), true, authz.RoleClinician)
	old := issuer
	old.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	s4 := signIn(t, u4, old)
	before := countAction(t, "intelligence.grant_refused", "token_expired")
	if a := mustAsk(t, ec, "echo", pingRequest(s4.tok, body)); a.code != "token_expired" {
		t.Errorf("expired token: %+v", a)
	}
	if n := countAction(t, "intelligence.grant_refused", "token_expired"); n != before+1 {
		t.Errorf("expired-token refusal audited %d times", n-before)
	}

	for _, c := range []struct {
		u      user
		reason string
	}{{u1, "unauthenticated"}, {u2, "unauthenticated"}, {u3, "mfa_required"}, {u5, "unauthenticated"}} {
		rows := auditRows(t, "intelligence.grant_refused", c.u.id)
		if len(rows) != 1 {
			t.Errorf("%s: %d refusal rows", c.reason, len(rows))
			continue
		}
		r := rows[0]
		if r.payload["reason"] != c.reason || r.payload["operation"] != "echo.ping" || r.hospital == nil || *r.hospital != c.u.hid.String() {
			t.Errorf("refusal row: %s", r.raw)
		}
		if r.requestID == nil || len(*r.requestID) != 32 {
			t.Errorf("refusal row request id: %v", r.requestID)
		}
	}
	text := allAuditText(t)
	for _, tok := range []string{s1.tok, s2.tok, s3.tok, s4.tok, s5.tok} {
		if strings.Contains(text, tok) || strings.Contains(logs.String(), tok) {
			t.Error("an access token reached the audit chain or the logs")
		}
	}
}

// TestGrantRefusesRepeatedRequestID (A36): the same BFF request id twice
// within 2 minutes; the second is forbidden.
func TestGrantRefusesRepeatedRequestID(t *testing.T) {
	startCore(t, copyOpts{})
	_, s := clinician(t)
	ec := echoConn(t)
	req := pingRequest(s.tok, []byte("x"))
	if a := mustAsk(t, ec, "echo", req); a.code != "" {
		t.Fatalf("first: %s", a.code)
	}
	if a := mustAsk(t, ec, "echo", req); a.code != "forbidden" {
		t.Fatalf("repeat: %+v", a)
	}
	req.BodyHash = bodyHash([]byte("another body")) // a new body doesn't make it new
	if a := mustAsk(t, ec, "echo", req); a.code != "forbidden" {
		t.Fatalf("repeat with another body: %+v", a)
	}
}

// TestGrantRefusesForeignReplyPrefix: reply_prefix _INBOX_gateway. (core's
// own inbox) is forbidden.
func TestGrantRefusesForeignReplyPrefix(t *testing.T) {
	startCore(t, copyOpts{})
	_, s := clinician(t)
	req := pingRequest(s.tok, []byte("x"))
	req.ReplyPrefix = "_INBOX_gateway."
	if a := mustAsk(t, echoConn(t), "echo", req); a.code != "forbidden" {
		t.Fatalf("gateway reply prefix: %+v", a)
	}
}

// TestGrantDropsForeignReplySubject (Task 4 ruling, a hard requirement): a
// grant request whose reply subject isn't the asking service's own inbox
// gets no answer, does no work and writes nothing: another service's
// inbox, a job on JOBS, a denylist write, a JetStream API call, wildcards.
// The service's own inbox works.
func TestGrantDropsForeignReplySubject(t *testing.T) {
	c := startCore(t, copyOpts{summary: 300 * time.Millisecond})
	u, s := clinician(t)
	ec, pc := echoConn(t), pipelineConn(t)
	bff := connect(t, natsSrv.BFFClinicalURL, bus.BFFInbox("clinical"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	subs := map[string]*nats.Subscription{}
	watch := func(name string, nc *nats.Conn, subject string) {
		sub, err := nc.SubscribeSync(subject)
		if err != nil {
			t.Fatal(err)
		}
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
		subs[name] = sub
	}
	watch("pipeline inbox", pc, bus.WorkerInbox("pipeline")+".>")
	watch("echo inbox", ec, bus.WorkerInbox("echo")+".>")
	watch("bff inbox", bff, bus.BFFInbox("clinical")+".>")
	watch("core inbox", c.nc, bus.GatewayInboxPrefix+".>")

	job := "jobs.v1." + uuid.NewString() + ".run_query"
	kvKey := "probe" + uniq()
	replies := []string{
		bus.WorkerInbox("pipeline") + ".x",
		job,
		"$KV." + bus.DenylistBucket + "." + kvKey,
		"$JS.API.STREAM.INFO." + bus.JobsStream,
		"$JS.API.x",
		bus.WorkerInbox("echo") + ".*",
		bus.WorkerInbox("echo") + ".>",
		bus.WorkerInbox("echo") + ".a*b",
		bus.WorkerInbox("echo") + "..x",
		bus.WorkerInbox("echo") + ".",
		// (a reply with a space is a NATS protocol error that closes the
		// sender's connection; TestReplyOK covers it)
		bus.WorkerInbox("echo") + "x.y",
		bus.WorkerInbox("echo"),
		bus.GatewayInboxPrefix + ".x",
		bus.BFFInbox("clinical") + ".x",
	}
	for _, reply := range replies {
		data, _ := proto.Marshal(pingRequest(s.tok, []byte("x")))
		m := nats.NewMsg(grant.SubjectPrefix + "echo")
		m.Reply = reply
		m.Data = data
		if err := ec.PublishMsg(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := ec.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)

	for name, sub := range subs {
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
	if got := c.metrics.DroppedReply.Load(); got != int64(len(replies)) {
		t.Errorf("dropped metric %d, want %d", got, len(replies))
	}
	if c.metrics.Granted.Load() != 0 || c.metrics.Refused.Load() != 0 {
		t.Errorf("work done: granted %d refused %d", c.metrics.Granted.Load(), c.metrics.Refused.Load())
	}
	if rows := auditRows(t, "intelligence.granted", u.id); len(rows) != 0 {
		t.Errorf("dropped requests audited: %d", len(rows))
	}
	logs := c.logs.String()
	// One sampled line per interval, and the count in the summary.
	if n := strings.Count(logs, "grant: reply subject refused"); n < 1 || n > 6 {
		t.Errorf("drops logged %d times: %s", n, logs)
	}
	if !strings.Contains(logs, `"msg":"grant: summary"`) || !strings.Contains(logs, fmt.Sprintf(`"dropped_reply":%d`, len(replies))) {
		t.Errorf("no summary with the drops: %s", logs)
	}
	for _, leak := range []string{"jobs.v1", "$KV", "$JS", kvKey, "_INBOX_"} {
		if strings.Contains(logs, leak) {
			t.Errorf("log carries the reply subject (%q)", leak)
		}
	}

	// The service's own inbox works.
	if a := mustAsk(t, ec, "echo", pingRequest(s.tok, []byte("x"))); a.code != "" {
		t.Fatalf("valid reply subject: %+v", a)
	}
}

// TestGrantedAudited: one intelligence.granted row with the operation,
// jti and bh, the user and hospital, under a request id core made.
func TestGrantedAudited(t *testing.T) {
	startCore(t, copyOpts{})
	u, s := clinician(t)
	body := []byte("synthetic audited body")
	req := pingRequest(s.tok, body)
	a := mustAsk(t, echoConn(t), "echo", req)
	if a.code != "" {
		t.Fatalf("refused: %s", a.code)
	}
	c, err := reqtoken.VerifyAt(a.token, signer.PublicKeys(), reqtoken.Expect{Aud: "echo", Subject: pingSubject, Kind: reqtoken.KindReq}, body, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, "intelligence.granted", u.id)
	if len(rows) != 1 {
		t.Fatalf("%d granted rows", len(rows))
	}
	r := rows[0]
	if r.payload["operation"] != "echo.ping" || r.payload["jti"] != c.Jti || r.payload["bh"] != c.BodyHash ||
		r.hospital == nil || *r.hospital != u.hid.String() || r.entityID == nil || *r.entityID != "echo.ping" {
		t.Fatalf("granted row: %s", r.raw)
	}
	if pids, ok := r.payload["patient_ids"].([]any); !ok || len(pids) != 0 {
		t.Fatalf("patient_ids: %s", r.raw)
	}
	if r.requestID == nil || len(*r.requestID) != 32 || *r.requestID == req.RequestId {
		t.Fatalf("request id %v: core must make its own", r.requestID)
	}
	if strings.Contains(r.raw, a.token) || strings.Contains(r.raw, s.tok) {
		t.Fatal("a token in the audit row")
	}
}

// TestTwoCoreCopiesShareGrants: two copies in queue group core each answer
// some of 100 grants, none twice. Then one copy is killed; retried
// grants are all answered by the other.
func TestTwoCoreCopiesShareGrants(t *testing.T) {
	a := startCore(t, copyOpts{limits: grant.Limits{PerSession: 1000, PerHospital: 1000}})
	b := startCore(t, copyOpts{limits: grant.Limits{PerSession: 1000, PerHospital: 1000}})
	_, s := clinician(t)
	ec := echoConn(t)
	var wg sync.WaitGroup
	var granted atomic.Int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ans, err := ask(t, ec, "echo", pingRequest(s.tok, []byte("x"))); err == nil && ans.code == "" {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	na, nb := a.metrics.Granted.Load(), b.metrics.Granted.Load()
	if granted.Load() != 100 || na+nb != 100 || na == 0 || nb == 0 {
		t.Fatalf("granted %d (copy a %d, copy b %d)", granted.Load(), na, nb)
	}
	if a.metrics.Refused.Load()+b.metrics.Refused.Load() != 0 {
		t.Fatal("a copy refused")
	}

	// Kill copy a. Every grant asked again is answered by b.
	a.stop()
	a.nc.Close()
	for i := 0; i < 20; i++ {
		var ans answer
		var err error
		for try := 0; try < 3; try++ { // the worker retries a lost grant
			if ans, err = ask(t, ec, "echo", pingRequest(s.tok, []byte("x"))); err == nil {
				break
			}
		}
		if err != nil || ans.code != "" {
			t.Fatalf("after the kill: %+v %v", ans, err)
		}
	}
	if b.metrics.Granted.Load() != nb+20 || a.metrics.Granted.Load() != na {
		t.Fatalf("after the kill: a %d b %d", a.metrics.Granted.Load(), b.metrics.Granted.Load())
	}
}

// TestGrantTokenNeverLogged: neither a marker access token, a real one,
// nor the minted request token appears in any log line, whatever the
// outcome.
func TestGrantTokenNeverLogged(t *testing.T) {
	logs := &syncBuffer{}
	g := newGuard(logs)
	startCore(t, copyOpts{guard: g, logs: logs})
	_, s := clinician(t)
	ec := echoConn(t)
	marker := "MARKER-ACCESS-TOKEN-" + uniq()
	for _, tok := range []string{marker, marker + ".x.y", s.tok} {
		req := pingRequest(tok, []byte("x"))
		_ = mustAsk(t, ec, "echo", req)
		req.Operation = "nope.nope"
		_ = mustAsk(t, ec, "echo", req)
	}
	a := mustAsk(t, ec, "echo", pingRequest(s.tok, []byte("x")))
	if a.code != "" {
		t.Fatalf("refused: %s", a.code)
	}
	out := logs.String()
	if out == "" {
		t.Fatal("nothing logged: the test proves nothing")
	}
	for _, secret := range []string{marker, s.tok, a.token} {
		if strings.Contains(out, secret) {
			t.Fatalf("a token in the logs: %s", out)
		}
	}
	if text := allAuditText(t); strings.Contains(text, marker) || strings.Contains(text, s.tok) || strings.Contains(text, a.token) {
		t.Fatal("a token on the audit chain")
	}
}

// TestGrantVerifiedRefusalAuditedPastJunkFlood (review, Important 1): a
// flood of tokens that don't verify fills the per-service audit cap, and a
// denylisted session's refusal right after is still audited.
func TestGrantVerifiedRefusalAuditedPastJunkFlood(t *testing.T) {
	var frozen atomic.Int64
	frozen.Store(time.Now().UnixNano())
	c := startCore(t, copyOpts{limits: grant.Limits{Now: func() time.Time { return time.Unix(0, frozen.Load()) }}})
	ec := echoConn(t)
	for i := 0; i < 60; i++ {
		if a := mustAsk(t, ec, "echo", pingRequest("junk.token."+strconv.Itoa(i), []byte("x"))); a.code != "unauthenticated" {
			t.Fatalf("junk token: %+v", a)
		}
	}
	if got := c.metrics.AuditSkipped.Load(); got != 40 {
		t.Fatalf("audit skipped %d, want 40 past the cap of 20", got)
	}
	u, s := clinician(t)
	if _, err := buckets.Denylist.Put(context.Background(), authz.SIDKey(s.sid.String()), []byte("1")); err != nil {
		t.Fatal(err)
	}
	if a := mustAsk(t, ec, "echo", pingRequest(s.tok, []byte("x"))); a.code != "unauthenticated" {
		t.Fatalf("denylisted session: %+v", a)
	}
	if rows := auditRows(t, "intelligence.grant_refused", u.id); len(rows) != 1 || rows[0].payload["reason"] != "unauthenticated" {
		t.Fatalf("verified-token refusal not audited past the junk flood: %+v", rows)
	}
	if got := c.metrics.AuditSkipped.Load(); got != 40 {
		t.Fatalf("audit skipped %d after the verified refusal", got)
	}
}

// TestGrantAuditFailureFreesRequestID (review, Minor 2): when the granted
// row can't be written, the grant is refused not_ready (retryable) and the
// BFF's request id is not used up: the retry is granted.
func TestGrantAuditFailureFreesRequestID(t *testing.T) {
	cfg := app.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	startCore(t, copyOpts{pool: pool})
	u, s := clinician(t)
	ec := echoConn(t)
	held, err := pool.Acquire(context.Background()) // the audit write can't get a connection
	if err != nil {
		t.Fatal(err)
	}
	req := pingRequest(s.tok, []byte("x"))
	data, _ := proto.Marshal(req)
	m, err := ec.Request(grant.SubjectPrefix+"echo", data, 15*time.Second)
	if err != nil {
		held.Release()
		t.Fatal(err)
	}
	held.Release()
	if a := decode(t, m); a.code != "not_ready" {
		t.Fatalf("audit down: %+v", a)
	}
	if rows := auditRows(t, "intelligence.granted", u.id); len(rows) != 0 {
		t.Fatalf("granted rows: %d", len(rows))
	}
	if a := mustAsk(t, ec, "echo", req); a.code != "" {
		t.Fatalf("retry with the same request id: %+v", a)
	}
	if a := mustAsk(t, ec, "echo", req); a.code != "forbidden" {
		t.Fatalf("a third use after the grant: %+v", a)
	}
}

// panicKV panics on every read.
type panicKV struct{ jetstream.KeyValue }

func (panicKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) { panic("synthetic") }

// TestGrantPanicAnswersNotReady (review, Minor 4): a panic in the handler
// answers not_ready at once, and the server keeps serving.
func TestGrantPanicAnswersNotReady(t *testing.T) {
	logs := &syncBuffer{}
	g := newGuard(logs)
	g.Denylist = panicKV{buckets.Denylist}
	c := startCore(t, copyOpts{guard: g, logs: logs})
	_, s := clinician(t)
	ec := echoConn(t)
	for i := 0; i < 2; i++ {
		start := time.Now()
		if a := mustAsk(t, ec, "echo", pingRequest(s.tok, []byte("x"))); a.code != "not_ready" || time.Since(start) > 2*time.Second {
			t.Fatalf("after a panic: %+v in %v", a, time.Since(start))
		}
	}
	if c.metrics.InFlight.Load() != 0 {
		t.Fatalf("in flight %d after panics", c.metrics.InFlight.Load())
	}
	if !strings.Contains(logs.String(), "grant: handler panicked") {
		t.Fatal("panic not logged")
	}
}
