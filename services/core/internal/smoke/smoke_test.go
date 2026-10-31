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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

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
}

// api sends one request over api.v1.clinical, as the BFF does.
func api(t *testing.T, nc *nats.Conn, method, path, bearer string, body any) reply {
	t.Helper()
	m := nats.NewMsg(natsapi.SubjectClinical)
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
	r := reply{status: status, raw: resp.Data}
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
	// POST /queries asks only for an active hospital (h.status = 'active'),
	// and nothing in core activates one yet (no CLI command or endpoint;
	// TODO.md). As the owner, the way an operator does it today.
	if _, err := pool.Exec(ctx, `UPDATE hospitals SET status = 'active' WHERE short_code = $1 AND status = 'setup_incomplete'`, hospitalCode); err != nil {
		t.Fatal(err)
	}
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
