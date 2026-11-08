//go:build smoke

// Package smoke is the end-to-end smoke test (Plan 3 Task 15, spec §12 and
// §17.1). It runs against the compose stack started by
// .github/scripts/boundary-smoke.sh (docker-compose.yml with
// docker-compose.smoke.yml): core, NATS, Postgres, the pipeline worker with
// the stub models, and the echo worker. It acts as the clinical BFF over
// NATS (bff_clinical on api.v1.clinical), as mhai-web's server will.
//
// Everything here is synthetic: hospital HAV, a clinician with a random
// address under example.test, a made-up question. The credentials are the
// ones the script generated for this run, read from the environment; none
// is printed.
package smoke

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	intelpb "github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/natsapi"
)

// The synthetic hospital the smoke clinician belongs to.
const hospitalCode = "HAV"

// env reads a required setting; the test can't run without the stack.
func env(t *testing.T, name string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Fatalf("%s is not set: run .github/scripts/boundary-smoke.sh", name)
	}
	return v
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// connect opens a NATS connection as user, with its own inbox prefix.
func connect(t *testing.T, user, password, inbox string) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(env(t, "SMOKE_NATS_URL"), nats.UserInfo(user, password),
		nats.CustomInboxPrefix(inbox), nats.MaxReconnects(0), nats.Name("smoke-"+user))
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func bffConn(t *testing.T) *nats.Conn {
	return connect(t, "bff_clinical", env(t, "SMOKE_NATS_BFF_CLINICAL_PASSWORD"), bus.BFFInboxPrefix+"."+natsapi.AppClinical)
}

func platformConn(t *testing.T) *nats.Conn {
	return connect(t, "bff_platform", env(t, "SMOKE_NATS_BFF_PLATFORM_PASSWORD"), bus.BFFInboxPrefix+"."+natsapi.AppPlatform)
}

func ownerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), env(t, "SMOKE_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// reply is one API answer.
type reply struct {
	status int
	body   map[string]any
	raw    []byte
	header nats.Header
}

// api sends one request over api.v1.clinical, as the BFF does.
func api(t *testing.T, nc *nats.Conn, method, path, bearer string, body any) reply {
	t.Helper()
	return apiOn(t, nc, natsapi.SubjectClinical, method, path, bearer, body)
}

// apiOn sends one request on subject: api.v1.clinical or api.v1.platform.
func apiOn(t *testing.T, nc *nats.Conn, subject, method, path, bearer string, body any) reply {
	t.Helper()
	m := nats.NewMsg(subject)
	m.Header.Set(natsapi.HeaderMethod, method)
	m.Header.Set(natsapi.HeaderPath, "/api/v1"+path)
	m.Header.Set("X-MHAI-Client-IP", "198.51.100.15") // TEST-NET-2
	if bearer != "" {
		m.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		m.Data = b
		m.Header.Set("Content-Type", "application/json")
	}
	resp, err := nc.RequestMsg(m, 20*time.Second)
	if err != nil {
		t.Fatalf("%s %s over NATS: %v", method, path, err)
	}
	status, err := strconv.Atoi(resp.Header.Get(natsapi.HeaderStatus))
	if err != nil {
		t.Fatalf("%s %s: no %s", method, path, natsapi.HeaderStatus)
	}
	r := reply{status: status, raw: resp.Data, header: resp.Header}
	if len(resp.Data) > 0 {
		_ = json.Unmarshal(resp.Data, &r.body)
	}
	return r
}

// clinician is a signed-in synthetic clinician of HAV.
type clinician struct {
	id, hospitalID, access string
}

// cli runs the core CLI (the provisioning login) through SMOKE_CORE_CLI,
// e.g. "docker compose ... run --rm --no-deps -T core-cli".
func cli(t *testing.T, stdin string, args ...string) {
	t.Helper()
	argv := append(strings.Fields(env(t, "SMOKE_CORE_CLI")), args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("core %s %s: %v\n%s", args[0], args[1], err, out)
	}
}

// signIn seeds HAV (once) and a new clinician with the CLI, then, over
// NATS: sign in, change the first password (the CLI sets
// must_change_password) and accept the terms, as the app makes a new user do.
func signIn(t *testing.T, nc *nats.Conn) clinician {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hospitals WHERE short_code = $1`, hospitalCode).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		cli(t, "", "hospitals", "create", "--name", "Synthetic Hospital HAV", "--short-code", hospitalCode, "--time-zone", "UTC")
	}
	// The CLI creates a hospital setup_incomplete. Sign-in accepts that, but
	// POST /queries asks for an active hospital: activate it with the CLI, as
	// the provisioning login. On a re-run it is already active, a no-op.
	cli(t, "", "hospitals", "activate", hospitalCode)
	email := "smoke-" + randomHex(t, 6) + "@hav.example.test"
	first, second := "Smoke-first-"+randomHex(t, 12), "Smoke-second-"+randomHex(t, 12)
	cli(t, first+"\n", "users", "create", "--email", email, "--name", "Synthetic Smoke Clinician", "--roles", "clinician", "--hospital", hospitalCode)

	r := api(t, nc, "POST", "/auth/login", "", map[string]any{"username": email, "password": first})
	if r.status != http.StatusOK {
		t.Fatalf("sign in: %d %s", r.status, r.raw)
	}
	access, _ := r.body["access_token"].(string)
	user, _ := r.body["user"].(map[string]any)
	hosp, _ := user["hospital"].(map[string]any)
	c := clinician{id: fmt.Sprint(user["id"]), hospitalID: fmt.Sprint(hosp["id"]), access: access}
	if access == "" || c.id == "" || c.hospitalID == "" {
		t.Fatalf("sign in: no token, user or hospital in the answer")
	}
	chainHas(t, "hospital.activated", "", c.hospitalID)
	if must, _ := user["must_change_password"].(bool); must {
		if r := api(t, nc, "POST", "/auth/password", access, map[string]any{"current_password": first, "new_password": second}); r.status != http.StatusNoContent {
			t.Fatalf("change password: %d %s", r.status, r.raw)
		}
	}
	if r := api(t, nc, "POST", "/auth/terms", access, map[string]any{"version": "2026-10"}); r.status != http.StatusNoContent {
		t.Fatalf("accept terms: %d %s", r.status, r.raw)
	}
	t.Logf("signed in a synthetic clinician of %s over api.v1.clinical", hospitalCode)
	return c
}

// question is the synthetic question the smoke test asks.
var question = map[string]any{
	"patient_data": map[string]any{
		"age": 54, "sex": "female", "symptoms": []string{"synthetic chest pain", "synthetic shortness of breath"},
	},
	"clinical_question": "Synthetic smoke-test question: what should be ruled out first?",
	"urgency":           "routine",
}

// ask posts the question with call and polls until it completes (60 s).
func ask(t *testing.T, call func(method, path string, body any) reply) string {
	t.Helper()
	r := call("POST", "/queries", question)
	if r.status != http.StatusAccepted {
		t.Fatalf("POST /queries: %d %s", r.status, r.raw)
	}
	id, _ := r.body["query_id"].(string)
	if id == "" || r.body["status"] != "queued" {
		t.Fatalf("POST /queries: %s", r.raw)
	}
	t.Logf("POST /queries: 202 queued")
	deadline := time.Now().Add(60 * time.Second)
	last := ""
	for {
		s := call("GET", "/queries/"+id, nil)
		if s.status != http.StatusOK {
			t.Fatalf("GET /queries/{id}: %d %s", s.status, s.raw)
		}
		status, _ := s.body["status"].(string)
		if status != last {
			t.Logf("GET /queries/{id}: %s", status)
			last = status
		}
		switch status {
		case "completed":
			res := call("GET", "/queries/"+id+"/result", nil)
			if res.status != http.StatusOK {
				t.Fatalf("GET /queries/{id}/result: %d %s", res.status, res.raw)
			}
			d, _ := res.body["decision"].(string)
			if d != "PASS" && d != "REGENERATE" && d != "ESCALATE" {
				t.Fatalf("result has no decision: %s", res.raw)
			}
			t.Logf("GET /queries/{id}/result: 200 decision=%s", d)
			return id
		case "failed":
			t.Fatalf("query failed: %s", s.raw)
		}
		if time.Now().After(deadline) {
			t.Fatalf("query still %s after 60 s", status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// chainHas waits (the drainer runs every few seconds) for an audit row and
// checks the whole chain.
func chainHas(t *testing.T, action, actor, entityID string) {
	t.Helper()
	ctx := context.Background()
	pool := ownerPool(t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1
			AND ($2 = '' OR actor_id::text = $2) AND ($3 = '' OR entity_id = $3)`, action, actor, entityID).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if n > 1 {
			t.Fatalf("%d %s rows, want 1", n, action)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s row after 30 s", action)
		}
		time.Sleep(500 * time.Millisecond)
	}
	ok, broken, err := audit.Verify(ctx, pool)
	if err != nil || !ok {
		t.Fatalf("audit chain: ok=%v first broken=%d err=%v", ok, broken, err)
	}
	t.Logf("audit chain verifies, with one %s row", action)
}

// TestQuestionEndToEnd: the BFF over NATS signs in, asks, polls and reads
// the answer the pipeline worker wrote (stub models), and the audit chain
// holds query.completed for it.
func TestQuestionEndToEnd(t *testing.T) {
	nc := bffConn(t)
	c := signIn(t, nc)
	id := ask(t, func(method, path string, body any) reply { return api(t, nc, method, path, c.access, body) })
	chainHas(t, "query.completed", "", id)
}

// TestEchoNeedsToken: a request to the echo worker with no token is refused
// unauthorized and does no work (spec §13.5 A1). Only a request from core's
// own inbox gets an answer before the token is checked, so this one is sent
// as the gateway user.
func TestEchoNeedsToken(t *testing.T) {
	nc := connect(t, "gateway", env(t, "SMOKE_NATS_GATEWAY_PASSWORD"), bus.GatewayInboxPrefix)
	body, _ := proto.Marshal(&intelpb.PingRequest{Text: "synthetic"})
	m := nats.NewMsg("inference.v1.echo.ping")
	m.Data = body
	m.Header.Set("Mhai-Schema", "mhai.intelligence.v1.PingRequest")
	m.Header.Set("Mhai-Request-Id", "smoke-"+randomHex(t, 8))
	resp := requestUntilAnswered(t, nc, m)
	if got := resp.Header.Get("Mhai-Status"); got != "unauthorized" {
		t.Fatalf("Mhai-Status = %q, want unauthorized", got)
	}
	var e intelpb.Error
	if err := proto.Unmarshal(resp.Data, &e); err != nil || e.GetCode() != "unauthorized" {
		t.Fatalf("Error = %v (%v)", &e, err)
	}
	t.Logf("echo.ping without a token: unauthorized")
}

// requestUntilAnswered retries while no worker is subscribed yet (the
// workers have no compose health check: their health is over NATS).
func requestUntilAnswered(t *testing.T, nc *nats.Conn, m *nats.Msg) *nats.Msg {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := nc.RequestMsg(m, 5*time.Second)
		if err == nil {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("no answer from %s: %v", m.Subject, err)
		}
		time.Sleep(time.Second)
	}
}

// TestBFFDirectEchoEndToEnd: the BFF calls echo.ping itself with the
// signed-in clinician's access token; the worker asks core for a grant,
// core grants it, the worker answers, and the chain holds
// intelligence.granted (spec §17.2, §17.3).
func TestBFFDirectEchoEndToEnd(t *testing.T) {
	nc := bffConn(t)
	c := signIn(t, nc)
	text := "synthetic smoke ping " + randomHex(t, 4)
	body, err := proto.Marshal(&intelpb.PingRequest{Text: text, HospitalId: c.hospitalID})
	if err != nil {
		t.Fatal(err)
	}
	m := nats.NewMsg("inference.v1.echo.ping")
	m.Data = body
	m.Header.Set("Authorization", "Bearer "+c.access)
	m.Header.Set("Mhai-Schema", "mhai.intelligence.v1.PingRequest")
	m.Header.Set("Mhai-Request-Id", "smoke-"+randomHex(t, 8))
	resp := requestUntilAnswered(t, nc, m)
	if got := resp.Header.Get("Mhai-Status"); got != "ok" {
		var e intelpb.Error
		_ = proto.Unmarshal(resp.Data, &e)
		t.Fatalf("Mhai-Status = %q (%s), want ok", got, e.GetCode())
	}
	var out intelpb.PingResponse
	if err := proto.Unmarshal(resp.Data, &out); err != nil || out.GetText() != text {
		t.Fatalf("PingResponse = %v (%v)", &out, err)
	}
	t.Logf("echo.ping via the grant: ok, text echoed")
	chainHas(t, "intelligence.granted", c.id, "echo.ping")
}

// TestQuestionOverHTTPStillWorks: with CORE_HTTP_API=on, the same question
// over HTTP (the BFF's service key and audience headers) still works, until
// mhai-web's BFF moves to NATS (spec §17.1).
func TestQuestionOverHTTPStillWorks(t *testing.T) {
	c := signIn(t, bffConn(t))
	base := env(t, "SMOKE_CORE_URL")
	key := env(t, "SMOKE_BFF_SERVICE_KEY")
	client := &http.Client{Timeout: 20 * time.Second}
	call := func(method, path string, body any) reply {
		var rd io.Reader = http.NoBody
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, base+"/api/v1"+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.access)
		req.Header.Set("X-MHAI-Service", key)
		req.Header.Set("X-MHAI-Audience", "clinical")
		req.Header.Set("X-MHAI-Client-IP", "198.51.100.16")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s over HTTP: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		r := reply{status: resp.StatusCode, raw: raw}
		_ = json.Unmarshal(raw, &r.body)
		return r
	}
	id := ask(t, call)
	chainHas(t, "query.completed", "", id)
}

// TestSmokeCorrectionApproved (Plan 4a): the two-person rule end to end.
// Clinician A asks and gets an answer, then suggests a correction. A can't
// approve it (403 self_approval); clinician B of the same hospital can. The
// approval queues approve_correction; the pipeline worker's learning
// consumer fills the buffer with exactly one row and core's drainer puts
// correction.applied on the chain, which still verifies.
func TestSmokeCorrectionApproved(t *testing.T) {
	nc := bffConn(t)
	cid, _, _ := approvedCorrection(t, nc)
	checkBuffer(t, cid)
}

// approvedCorrection runs the two-person flow: A asks, suggests a
// correction and fails to approve it; B approves. It returns the correction
// id and both clinicians. The caller waits for correction.applied.
func approvedCorrection(t *testing.T, nc *nats.Conn) (string, clinician, clinician) {
	t.Helper()
	a := signIn(t, nc)
	b := signIn(t, nc)
	if a.hospitalID != b.hospitalID {
		t.Fatalf("A and B are in different hospitals")
	}
	qid := ask(t, func(method, path string, body any) reply { return api(t, nc, method, path, a.access, body) })

	r := api(t, nc, "POST", "/feedback", a.access, map[string]any{
		"query_id": qid, "action": "correct",
		"corrected_diagnosis": "Synthetic corrected diagnosis",
		"reason":              "Synthetic smoke-test reason for the correction.",
	})
	if r.status != http.StatusOK || r.body["status"] != "pending_review" {
		t.Fatalf("POST /feedback: %d %s", r.status, r.raw)
	}
	cid, _ := r.body["correction_id"].(string)
	if cid == "" {
		t.Fatalf("POST /feedback: no correction_id: %s", r.raw)
	}
	t.Logf("POST /feedback: pending_review")

	r = api(t, nc, "POST", "/corrections/"+cid+"/approve", a.access, nil)
	if r.status != http.StatusForbidden || r.body["code"] != "self_approval" {
		t.Fatalf("A approving own correction: %d %s, want 403 self_approval", r.status, r.raw)
	}
	t.Logf("A approving own correction: 403 self_approval")

	r = api(t, nc, "POST", "/corrections/"+cid+"/approve", b.access, nil)
	if r.status != http.StatusNoContent {
		t.Fatalf("B approving: %d %s, want 204", r.status, r.raw)
	}
	t.Logf("B approving: 204")
	return cid, a, b
}

func checkBuffer(t *testing.T, cid string) {
	t.Helper()
	// chainHas waits for the one row and verifies the whole chain.
	chainHas(t, "correction.applied", "", cid)

	var rows int
	if err := ownerPool(t).QueryRow(context.Background(),
		`SELECT count(*) FROM feedback_buffer WHERE correction_id = $1`, cid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d feedback_buffer rows for the correction, want 1", rows)
	}
	ok, broken, err := audit.Verify(context.Background(), ownerPool(t))
	if err != nil || !ok {
		t.Fatalf("audit chain: ok=%v first broken=%d err=%v", ok, broken, err)
	}
	t.Logf("one feedback_buffer row; audit chain verifies")
}

// totpFor is the RFC 6238 code (SHA-1, 30 s, 6 digits) for a step.
func totpFor(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// auditorSignIn is platformSignIn for an auditor.
func auditorSignIn(t *testing.T, nc *nats.Conn) string {
	t.Helper()
	return platformSignIn(t, nc, "auditor")
}

// platformSignIn creates a synthetic platform user with role (auditor or
// platform_admin) with the CLI and signs in over
// api.v1.platform: the password step, an enrolment-only session, two-step
// enrolment, then a full session with a code (the next 30 s step, which the
// verifier accepts and which is later than the enrolment code).
func platformSignIn(t *testing.T, nc *nats.Conn, role string) string {
	t.Helper()
	email := "smoke-" + role + "-" + randomHex(t, 6) + "@platform.example.test"
	pw := "Smoke-platform-" + randomHex(t, 12)
	cli(t, pw+"\n", "users", "create", "--email", email, "--name", "Synthetic Smoke Platform User", "--roles", role)
	call := func(method, path, bearer string, body any) reply {
		return apiOn(t, nc, natsapi.SubjectPlatform, method, path, bearer, body)
	}
	login := func() string {
		r := call("POST", "/auth/login", "", map[string]any{"username": email, "password": pw})
		tok, _ := r.body["mfa_token"].(string)
		if r.status != http.StatusOK || r.body["mfa_required"] != true || tok == "" {
			t.Fatalf("auditor sign in: %d %s, want the two-step branch", r.status, r.raw)
		}
		return tok
	}
	r := call("POST", "/auth/mfa/challenge", "", map[string]any{"mfa_token": login(), "code": ""})
	limited, _ := r.body["access_token"].(string)
	if r.status != http.StatusOK || limited == "" {
		t.Fatalf("auditor enrolment session: %d %s", r.status, r.raw)
	}
	r = call("POST", "/auth/mfa/enroll", limited, nil)
	uri, _ := r.body["otpauth_uri"].(string)
	u, err := url.Parse(uri)
	if r.status != http.StatusOK || err != nil {
		t.Fatalf("auditor enrol: %d", r.status)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(u.Query().Get("secret"))
	if err != nil || len(secret) == 0 {
		t.Fatalf("auditor enrol: no secret")
	}
	step := time.Now().Unix() / 30
	if r := call("POST", "/auth/mfa/verify", limited, map[string]any{"code": totpFor(secret, step)}); r.status != http.StatusNoContent {
		t.Fatalf("auditor mfa verify: %d %s", r.status, r.raw)
	}
	r = call("POST", "/auth/mfa/challenge", "", map[string]any{"mfa_token": login(), "code": totpFor(secret, step+1)})
	access, _ := r.body["access_token"].(string)
	if r.status != http.StatusOK || access == "" {
		t.Fatalf("auditor challenge: %d %s", r.status, r.raw)
	}
	t.Logf("signed in a synthetic %s over api.v1.platform (two-step)", role)
	return access
}

// TestSmokeAuditLog (Plan 4b): a synthetic auditor over api.v1.platform
// starts a chain check and polls it to intact, reads the integrity status
// (which needs the stack's synthetic security contact), and finds clinician
// B's review_submitted in the log filtered to type=review.
func TestSmokeAuditLog(t *testing.T) {
	clinical := bffConn(t)
	cid, _, b := approvedCorrection(t, clinical)
	chainHas(t, "correction.applied", "", cid)

	nc := platformConn(t)
	access := auditorSignIn(t, nc)
	call := func(method, path string, body any) reply {
		return apiOn(t, nc, natsapi.SubjectPlatform, method, path, access, body)
	}

	r := call("POST", "/platform/audit/verify", nil)
	runID, _ := r.body["run_id"].(string)
	if r.status != http.StatusOK || runID == "" {
		t.Fatalf("POST /platform/audit/verify: %d %s", r.status, r.raw)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		r = call("GET", "/platform/audit/verify/runs/"+runID, nil)
		if r.status != http.StatusOK {
			t.Fatalf("GET run: %d %s", r.status, r.raw)
		}
		state, _ := r.body["state"].(string)
		if state == "intact" {
			break
		}
		if state != "running" {
			t.Fatalf("chain check ended %q, want intact: %s", state, r.raw)
		}
		if time.Now().After(deadline) {
			t.Fatalf("chain check still running after 90 s")
		}
		time.Sleep(time.Second)
	}
	if n, _ := r.body["entries_checked"].(float64); n <= 0 {
		t.Fatalf("intact check with entries_checked %v", r.body["entries_checked"])
	}
	t.Logf("chain check %s: intact", runID)

	r = call("GET", "/platform/audit/verify/status", nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /platform/audit/verify/status: %d %s (is SECURITY_CONTACT_NAME/EMAIL set in the stack?)", r.status, r.raw)
	}
	t.Logf("integrity status: 200")

	r = call("GET", "/platform/audit?type=review&limit=100", nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /platform/audit?type=review: %d %s", r.status, r.raw)
	}
	events, _ := r.body["events"].([]any)
	found := false
	for _, e := range events {
		ev, _ := e.(map[string]any)
		actor, _ := ev["actor"].(map[string]any)
		if ev["type"] != "review" {
			t.Fatalf("type=review returned a %v entry", ev["type"])
		}
		if ev["kind"] == "review_submitted" && actor["id"] == b.id {
			found = true
		}
	}
	if !found {
		t.Fatalf("no review_submitted by clinician B among %d review entries: %s", len(events), r.raw)
	}
	t.Logf("audit log: B's review_submitted is listed under type=review")
}

// guardedOK checks one guarded figure's shape and that a shown integer is
// not 1 to 4 (the small-group rule).
func guardedOK(t *testing.T, where string, v any, share bool) {
	t.Helper()
	g, ok := v.(map[string]any)
	if !ok || len(g) != 2 {
		t.Fatalf("guardrails %s: %v, want {value, suppressed}", where, v)
	}
	sup, ok := g["suppressed"].(bool)
	if !ok {
		t.Fatalf("guardrails %s: no suppressed flag: %v", where, g)
	}
	val, present := g["value"]
	if !present {
		t.Fatalf("guardrails %s: no value key: %v", where, g)
	}
	if sup {
		if val != nil {
			t.Fatalf("guardrails %s: suppressed with a value: %v", where, g)
		}
		return
	}
	if val == nil && share {
		return // a share of no answers: nothing to divide, nothing hidden
	}
	n, ok := val.(float64)
	if !ok {
		t.Fatalf("guardrails %s: shown without a number: %v", where, g)
	}
	if share {
		if n < 0 || n > 1 {
			t.Fatalf("guardrails %s: share %v out of range", where, n)
		}
	} else if n >= 1 && n <= 4 {
		t.Fatalf("guardrails %s: shows %v, a count of 1 to 4", where, n)
	}
}

func ratesOK(t *testing.T, where string, m map[string]any) {
	t.Helper()
	for _, k := range []string{"answers", "pass", "regenerate", "escalate", "degraded"} {
		guardedOK(t, where+"."+k, m[k], false)
	}
	for _, k := range []string{"pass_share", "regenerate_share", "escalate_share"} {
		guardedOK(t, where+"."+k, m[k], true)
	}
}

// TestSmokePlatformOps (Plan 4c): a synthetic platform admin over
// api.v1.platform reads system health once the sampler has taken its first
// reading (fresh, workers ok), finds a forced dead letter in the failed jobs
// list and retries it, and reads the guardrail statistics for 7 days: the
// right shape, no shown count of 1 to 4.
func TestSmokePlatformOps(t *testing.T) {
	clinical := bffConn(t)
	creator := signIn(t, clinical)
	nc := platformConn(t)
	access := platformSignIn(t, nc, "platform_admin")
	call := func(method, path string, body any) reply {
		return apiOn(t, nc, natsapi.SubjectPlatform, method, path, access, body)
	}

	// Health: 503 until the sampler's first reading, and a reading taken
	// before the workers were up says so; wait (the sampler runs every
	// 30 s) for a fresh reading with the workers ok.
	var r reply
	workers := ""
	deadline := time.Now().Add(120 * time.Second)
	for {
		r = call("GET", "/platform/health", nil)
		if r.status != http.StatusOK && r.status != http.StatusServiceUnavailable {
			t.Fatalf("GET /platform/health: %d %s", r.status, r.raw)
		}
		workers = ""
		parts, _ := r.body["parts"].([]any)
		for _, p := range parts {
			if m, _ := p.(map[string]any); m["key"] == "workers" {
				workers, _ = m["status"].(string)
			}
		}
		if r.status == http.StatusOK && workers == "ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /platform/health: %d, workers %q after 120 s, want 200 and ok: %s", r.status, workers, r.raw)
		}
		time.Sleep(3 * time.Second)
	}
	updated, err := time.Parse(time.RFC3339, fmt.Sprint(r.body["updated_at"]))
	if err != nil || time.Since(updated) > 2*time.Minute || time.Until(updated) > 30*time.Second {
		t.Fatalf("health reading not fresh: updated_at %v", r.body["updated_at"])
	}
	t.Logf("health: fresh (%s), workers ok, overall %v", updated.Format(time.RFC3339), r.body["overall"])

	// A dead letter: a failed question of the clinician's hospital.
	qid := uuid.NewString()
	if _, err := ownerPool(t).Exec(context.Background(), `INSERT INTO clinical_queries
		(id, hospital_id, clinical_question, urgency, status, error, attempts, created_by, request_id, created_at, completed_at, deadline_at)
		VALUES ($1, $2, $3, 'routine', 'failed', 'deadline_exceeded', 5, $4, $5, now() - interval '1 hour', now() - interval '1 minute',
		        now() - interval '50 minutes')`,
		qid, creator.hospitalID, "Synthetic smoke dead letter "+qid, creator.id, "rid-q-"+qid); err != nil {
		t.Fatal(err)
	}
	jobID := "q_" + qid
	r = call("GET", "/platform/jobs?status=failed&hospital="+creator.hospitalID, nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /platform/jobs: %d %s", r.status, r.raw)
	}
	listed := false
	jobs, _ := r.body["jobs"].([]any)
	for _, x := range jobs {
		if j, _ := x.(map[string]any); j["id"] == jobID {
			listed = j["status"] == "failed"
		}
	}
	if !listed {
		t.Fatalf("the dead letter %s is not among %d failed jobs: %s", jobID, len(jobs), r.raw)
	}
	t.Logf("jobs: the dead letter is listed as failed")

	r = call("POST", "/platform/jobs/retry", map[string]any{"ids": []string{jobID}})
	retried, _ := r.body["retried"].([]any)
	if r.status != http.StatusOK || len(retried) != 1 || retried[0] != jobID {
		t.Fatalf("POST /platform/jobs/retry: %d %s, want %s retried", r.status, r.raw, jobID)
	}
	t.Logf("jobs: retried")

	// Guardrails (Plan 7c, R41a-3, R41a-4): the deprecated rolling route
	// answers in its old shape with every figure hidden and the deprecation
	// headers; the calendar periods route answers 503 not_ready until its
	// first period after the release is published, and then the shape.
	r = call("GET", "/platform/guardrails?period=7d", nil)
	if r.status != http.StatusOK {
		t.Fatalf("GET /platform/guardrails: %d %s", r.status, r.raw)
	}
	if r.body["period"] != "7d" || r.body["from"] == nil || r.body["to"] == nil || r.body["updated_at"] == nil {
		t.Fatalf("guardrails: missing period, from, to or updated_at: %s", r.raw)
	}
	if r.header.Get("Deprecation") == "" || r.header.Get("Sunset") == "" ||
		!strings.Contains(r.header.Get("Link"), "/docs/notices/guardrails-rolling-windows") {
		t.Fatalf("guardrails 7d: no deprecation headers: %v", r.header)
	}
	if strings.Contains(string(r.raw), `"suppressed":false`) {
		t.Fatalf("guardrails 7d: a figure is shown: %s", r.raw)
	}
	all, _ := r.body["all"].(map[string]any)
	ratesOK(t, "all", all)
	hospitals, ok := r.body["hospitals"].([]any)
	if !ok {
		t.Fatalf("guardrails: hospitals is not a list: %s", r.raw)
	}
	for i, h := range hospitals {
		m, _ := h.(map[string]any)
		if _, ok := m["hospital"].(map[string]any); !ok {
			t.Fatalf("guardrails hospitals[%d]: no hospital: %v", i, m)
		}
		ratesOK(t, fmt.Sprintf("hospitals[%d]", i), m)
	}
	t.Logf("guardrails: 7d deprecated and fully hidden, %d hospital rows", len(hospitals))
	for _, k := range []string{"week", "month", "quarter"} {
		r = call("GET", "/platform/guardrails/periods?period="+k, nil)
		switch {
		case r.status == http.StatusServiceUnavailable && r.body["code"] == "not_ready":
			t.Logf("guardrails/periods %s: not published yet (not_ready)", k)
		case r.status == http.StatusOK:
			if r.body["period"] != k || r.header.Get("Deprecation") != "" {
				t.Fatalf("guardrails/periods %s: %s %v", k, r.raw, r.header)
			}
			all, _ := r.body["all"].(map[string]any)
			ratesOK(t, k+" all", all)
			t.Logf("guardrails/periods %s: published", k)
		default:
			t.Fatalf("GET /platform/guardrails/periods?period=%s: %d %s", k, r.status, r.raw)
		}
	}

	// Analytics (Plan 7c-2): the month is either not yet published (every
	// area {not_yet, first_from}) or the latest stored month; alerts null,
	// changes unavailable until 7c-3. One hospital's series: a hospital of
	// the published month, or 404 for one in no published period.
	r = call("GET", "/platform/analytics?period=month", nil)
	if r.status != http.StatusOK || r.body["period"] != "month" || r.body["alerts"] != nil {
		t.Fatalf("GET /platform/analytics?period=month: %d %s", r.status, r.raw)
	}
	if c, _ := r.body["changes"].(map[string]any); c["unavailable"] != true {
		t.Fatalf("analytics: changes %v, want unavailable until 7c-3", r.body["changes"])
	}
	seriesID := uuid.NewString()
	use, _ := r.body["use"].(map[string]any)
	switch {
	case r.body["from"] == nil && use["not_yet"] == true:
		t.Logf("analytics month: not yet published (first %v)", use["first_from"])
	case r.body["from"] != nil && use["rows"] != nil:
		rows, _ := use["rows"].([]any)
		for _, x := range rows {
			if m, _ := x.(map[string]any); m != nil {
				h, _ := m["hospital"].(map[string]any)
				seriesID, _ = h["id"].(string)
				break
			}
		}
		if raw := string(r.raw); strings.Contains(raw, `"value":1,`) || strings.Contains(raw, `"value":2,`) ||
			strings.Contains(raw, `"value":3,`) || strings.Contains(raw, `"value":4,`) {
			t.Fatalf("analytics month: a count of 1 to 4 is shown: %s", r.raw)
		}
		t.Logf("analytics month: published, %d hospital rows", len(rows))
	case r.body["from"] != nil && use["unavailable"] == true:
		t.Logf("analytics month: published, snapshot unavailable")
	default:
		t.Fatalf("analytics month: %s", r.raw)
	}
	r = call("GET", "/platform/analytics/hospitals/"+seriesID+"/series?measure=needs_review_share&period=month", nil)
	switch {
	case r.status == http.StatusOK:
		pts, _ := r.body["points"].([]any)
		if r.body["measure"] != "needs_review_share" || len(pts) == 0 || len(pts) > 12 {
			t.Fatalf("hospital series: %s", r.raw)
		}
		t.Logf("hospital series: %d monthly points", len(pts))
	case r.status == http.StatusNotFound && r.body["code"] == "not_found":
		t.Logf("hospital series: no published month lists the hospital (not_found)")
	default:
		t.Fatalf("GET hospital series: %d %s", r.status, r.raw)
	}
}
