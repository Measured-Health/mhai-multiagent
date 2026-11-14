//go:build integration

package platform_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/reqtoken"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// EMR plan Task 11: the platform connection routes through core's whole
// chain, against a fake emr service on NATS (the emr NATS user) that
// verifies core's tokens and opens the sealed boxes with the test key.
// Every id, name, address and secret here is synthetic.

// testIdemKey is the idempotency HMAC key of the test deployment.
const testIdemKey = "synthetic-idempotency-hmac-key-0123456789abcdef"

var (
	emrKP  seal.KeyPair
	fake   *fakeEMR
	emrOff http.Handler
)

// fakeEMR answers emr.v1.ctl.* as the emr service would, for core's side:
// it checks the token (and stp on step-up ops), opens every box against the
// connection's stored origin, and keeps the values in memory, writing the
// metadata rows the real service writes. Every message it receives is kept
// (the NATS tap of A1).
type fakeEMR struct {
	nc *nats.Conn
	kp seal.KeyPair

	mu     sync.Mutex
	calls  []fakeCall
	values map[string]string // connection/field -> value
	seen   [][]byte

	wipeFails atomic.Bool
	putFails  atomic.Bool
	// onPut, when set, runs as a put arrives (before it is answered).
	onPut func(conn string)
}

type fakeCall struct {
	op, sub, hid string
	stepUp       bool
	conn         string
}

func startFakeEMR(url string, kp seal.KeyPair) (*fakeEMR, error) {
	f := &fakeEMR{kp: kp, values: map[string]string{}}
	nc, err := nats.Connect(url, nats.CustomInboxPrefix("_INBOX_worker.emr"))
	if err != nil {
		return nil, err
	}
	f.nc = nc
	if _, err := nc.QueueSubscribe("emr.v1.ctl.>", "emr", f.handle); err != nil {
		nc.Close()
		return nil, err
	}
	return f, nc.Flush()
}

func (f *fakeEMR) handle(m *nats.Msg) {
	var hdr bytes.Buffer
	for k, vs := range m.Header {
		hdr.WriteString(k + ":" + strings.Join(vs, ",") + "\n")
	}
	f.mu.Lock()
	f.seen = append(f.seen, append(hdr.Bytes(), m.Data...))
	f.mu.Unlock()
	c, err := reqtoken.VerifyAt(m.Header.Get("Mhai-Request-Token"), reqSigner.PublicKeys(),
		reqtoken.Expect{Aud: "emr", Subject: m.Subject, Kind: reqtoken.KindReq}, m.Data, time.Now())
	if err != nil || reqtoken.CheckReply(c, m.Reply, false) != nil {
		return
	}
	op, ok := emrops.Lookup(m.Subject)
	if !ok || !op.Allows(c.Sub, c.Roles) || (op.StepUp && !c.StepUp) {
		return
	}
	var rep proto.Message
	var conn string
	switch op.Op {
	case "put_credentials":
		var req emrpb.PutCredentialsRequest
		if proto.Unmarshal(m.Data, &req) != nil {
			return
		}
		conn = req.GetConnectionId()
		rep = f.put(c, &req)
	case "wipe_credentials":
		var req emrpb.WipeCredentialsRequest
		if proto.Unmarshal(m.Data, &req) != nil {
			return
		}
		conn = req.GetConnectionId()
		rep = f.wipe(c, req.GetConnectionId())
	case "test":
		var req emrpb.TestRequest
		if proto.Unmarshal(m.Data, &req) != nil {
			return
		}
		conn = req.GetConnectionId()
		rep = f.test(c, req.GetConnectionId())
	case "sync_now":
		var req emrpb.SyncNowRequest
		if proto.Unmarshal(m.Data, &req) != nil {
			return
		}
		conn = req.GetConnectionId()
		rep = &emrpb.SyncNowReply{Queued: true}
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{op: op.Op, sub: c.Sub, hid: c.Hid, stepUp: c.StepUp, conn: conn})
	f.mu.Unlock()
	if rep == nil {
		return
	}
	b, _ := proto.Marshal(rep)
	out := nats.NewMsg(m.Reply)
	out.Data = b
	out.Header.Set("Mhai-Schema", op.Response)
	out.Header.Set("Mhai-Request-Id", c.Rid)
	_ = m.RespondMsg(out)
}

// origin is the connection's stored origin under the token's hospital.
func (f *fakeEMR) origin(hid, conn string) ([]emrurl.Origin, bool) {
	var origin []string
	if err := super.QueryRow(context.Background(), `SELECT credentials_origin FROM emr_connections WHERE id = $1 AND hospital_id = $2`,
		conn, hid).Scan(&origin); err != nil {
		return nil, false
	}
	var out []emrurl.Origin
	for _, o := range origin {
		out = append(out, emrurl.Origin(o))
	}
	return out, true
}

func (f *fakeEMR) put(c reqtoken.Claims, req *emrpb.PutCredentialsRequest) proto.Message {
	f.mu.Lock()
	hook := f.onPut
	f.mu.Unlock()
	if hook != nil {
		hook(req.GetConnectionId())
	}
	if f.putFails.Load() {
		return &emrpb.PutCredentialsReply{} // confirms nothing
	}
	origin, ok := f.origin(c.Hid, req.GetConnectionId())
	if !ok {
		return nil
	}
	keys := seal.Keys{seal.KeyID(f.kp.Public[:]): f.kp}
	conn := uuid.MustParse(req.GetConnectionId())
	opened := map[string]string{}
	for _, fl := range req.GetFields() {
		v, err := seal.Open(keys, fl.GetBox(), seal.Binding{ConnectionID: conn, Field: fl.GetField(), Origin: origin}, time.Now())
		if err != nil {
			return nil
		}
		opened[fl.GetField()] = string(v.Bytes())
	}
	now := time.Now().UTC().Truncate(time.Second)
	rep := &emrpb.PutCredentialsReply{}
	for k, v := range opened {
		if _, err := super.Exec(context.Background(), `INSERT INTO emr.credentials (connection_id, hospital_id, field, set_at,
			secret_version, set_by, origin) VALUES ($1, $2, $3, $4, 'v1', $5, $6)
			ON CONFLICT (connection_id, field) DO UPDATE SET set_at = EXCLUDED.set_at, origin = EXCLUDED.origin`,
			conn, c.Hid, k, now, c.Sub, emrurl.Strings(origin)); err != nil {
			return nil
		}
		f.mu.Lock()
		f.values[conn.String()+"/"+k] = v
		f.mu.Unlock()
		rep.Fields = append(rep.Fields, &emrpb.FieldSetAt{Field: k, SetAt: now.Format(time.RFC3339)})
	}
	return rep
}

func (f *fakeEMR) wipe(c reqtoken.Claims, conn string) proto.Message {
	if f.wipeFails.Load() {
		return &emrpb.WipeCredentialsReply{Wiped: false}
	}
	if _, ok := f.origin(c.Hid, conn); !ok {
		return nil
	}
	if _, err := super.Exec(context.Background(), `DELETE FROM emr.credentials WHERE connection_id = $1`, conn); err != nil {
		return nil
	}
	f.mu.Lock()
	for k := range f.values {
		if strings.HasPrefix(k, conn+"/") {
			delete(f.values, k)
		}
	}
	f.mu.Unlock()
	return &emrpb.WipeCredentialsReply{Wiped: true}
}

// test passes with the stub's key set for the current origin and not
// "fail"; otherwise sign_in_refused.
func (f *fakeEMR) test(c reqtoken.Claims, conn string) proto.Message {
	origin, ok := f.origin(c.Hid, conn)
	if !ok {
		return nil
	}
	var stored []string
	err := super.QueryRow(context.Background(), `SELECT origin FROM emr.credentials WHERE connection_id = $1 AND field = 'api_key'`,
		conn).Scan(&stored)
	f.mu.Lock()
	v, set := f.values[conn+"/api_key"]
	f.mu.Unlock()
	if err != nil || !set || !slices.Equal(stored, emrurl.Strings(origin)) || v == "fail" {
		return &emrpb.TestReply{Ok: false, Code: "sign_in_refused"}
	}
	return &emrpb.TestReply{Ok: true, Code: "ok"}
}

func (f *fakeEMR) callsFor(conn uuid.UUID) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.conn == conn.String() {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeEMR) value(conn uuid.UUID, field string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[conn.String()+"/"+field]
	return v, ok
}

// --- helpers ---------------------------------------------------------------

func connBody(t *testing.T, kv map[string]any) string {
	t.Helper()
	m := map[string]any{"type": "stub", "source_label": "Main record", "base_url": "https://stub.example.org/api",
		"settings": map[string]string{"client_id": "synthetic-client"}, "credentials": map[string]string{"api_key": "synthetic-key-a"}}
	for k, v := range kv {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func createConn(t *testing.T, tok string, hid uuid.UUID, body string) resp {
	t.Helper()
	return send(t, handler, http.MethodPost, "/platform/hospitals/"+hid.String()+"/emr/connections", tok, body)
}

func connPath(id string) string { return "/platform/emr/connections/" + id }

func patchConn(t *testing.T, tok, id, body string) resp {
	t.Helper()
	return send(t, handler, http.MethodPatch, connPath(id), tok, body)
}

func version(t *testing.T, r resp) int {
	t.Helper()
	v, ok := r.body["version"].(float64)
	if !ok {
		t.Fatalf("no version: %s", r.raw)
	}
	return int(v)
}

func creds(r resp) map[string]any {
	out := map[string]any{}
	list, _ := r.body["credentials"].([]any)
	for _, c := range list {
		m := c.(map[string]any)
		out[m["key"].(string)] = m["set_at"]
	}
	return out
}

func mustJSONs(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type emrAudit struct {
	action, actor, hospital string
	payload                 map[string]any
}

func connAudits(t *testing.T, conn string) []emrAudit {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, COALESCE(actor_id::text, ''), COALESCE(hospital_id::text, ''), payload
		FROM audit_log WHERE entity = 'emr_connection' AND entity_id = $1 ORDER BY id`, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []emrAudit
	for rows.Next() {
		var a emrAudit
		var raw []byte
		if err := rows.Scan(&a.action, &a.actor, &a.hospital, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

// --- tests -----------------------------------------------------------------

// The lifecycle: types, create (off, credentials set, untested), list and
// read, test, switch on, a label edit (keeps the test), a credential edit
// (same origin: stays on, clears the test), a path change on the same host
// (no wipe), and every write audited with ids and codes.
func TestEMRConnectionLifecycleRoutes(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")

	types := get(t, "/platform/emr/connector-types", tok)
	ok(t, "types", types)
	if got := mustJSONs(types.body["types"]); !strings.Contains(got, `"id":"stub"`) || strings.Contains(got, "fhir_r4") ||
		!strings.Contains(got, `"kind":"secret"`) {
		t.Fatalf("types: %s", got)
	}

	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	if c.body["enabled"] != false || c.body["health"] != "never" || c.body["last_test"] != nil || version(t, c) != 1 ||
		c.body["type_label"] != "Test connection" || c.body["hospital"].(map[string]any)["id"] != hid.String() {
		t.Fatalf("created: %s", c.raw)
	}
	if setAt := creds(c)["api_key"]; setAt == nil {
		t.Fatalf("credential not set: %s", c.raw)
	}
	if v, _ := fake.value(uuid.MustParse(id), "api_key"); v != "synthetic-key-a" {
		t.Fatalf("the emr service holds %q", v)
	}
	if c.body["settings"].(map[string]any)["client_id"] != "synthetic-client" || c.body["base_url"] != "https://stub.example.org/api" {
		t.Fatalf("settings: %s", c.raw)
	}
	calls := fake.callsFor(uuid.MustParse(id))
	if len(calls) != 1 || calls[0].op != "put_credentials" || !calls[0].stepUp || calls[0].sub != admin.id.String() ||
		calls[0].hid != hid.String() {
		t.Fatalf("calls %+v", calls)
	}

	list := get(t, "/platform/emr/connections?hospital="+hid.String(), tok)
	ok(t, "list", list)
	conns := list.body["connections"].([]any)
	if len(conns) != 1 || len(list.body["not_connected"].([]any)) != 0 {
		t.Fatalf("list: %s", list.raw)
	}
	for _, k := range []string{"base_url", "settings", "credentials"} {
		if _, has := conns[0].(map[string]any)[k]; has {
			t.Errorf("a summary carries %s", k)
		}
	}
	// Another hospital with none is not connected.
	other := seedHospital(t, "active")
	nc := get(t, "/platform/emr/connections?state=not_connected&hospital="+other.String(), tok)
	ok(t, "not connected", nc)
	if len(nc.body["connections"].([]any)) != 0 || len(nc.body["not_connected"].([]any)) != 1 {
		t.Fatalf("not connected: %s", nc.raw)
	}

	tested := send(t, handler, http.MethodPost, connPath(id)+"/test", tok, "")
	ok(t, "test", tested)
	lt := tested.body["last_test"].(map[string]any)
	if lt["ok"] != true || lt["code"] != nil || version(t, tested) != 1 {
		t.Fatalf("tested: %s", tested.raw)
	}

	on := send(t, handler, http.MethodPut, connPath(id)+"/enabled", tok,
		mustJSONs(map[string]any{"enabled": true, "version": 1, "backup_code": backupCode(t, admin)}))
	ok(t, "on", on)
	if on.body["enabled"] != true || version(t, on) != 2 {
		t.Fatalf("on: %s", on.raw)
	}
	// Switching on asks the emr service to poll now, as core (svc:core),
	// for the connection's hospital.
	if calls := fake.callsFor(uuid.MustParse(id)); len(calls) != 3 || calls[2].op != "sync_now" ||
		calls[2].sub != "svc:core" || calls[2].hid != hid.String() {
		t.Fatalf("calls after switching on %+v", calls)
	}

	renamed := patchConn(t, tok, id, mustJSONs(map[string]any{"source_label": "Results system", "version": 2,
		"backup_code": backupCode(t, admin)}))
	ok(t, "rename", renamed)
	if renamed.body["source_label"] != "Results system" || renamed.body["last_test"] == nil || renamed.body["enabled"] != true {
		t.Fatalf("rename: %s", renamed.raw)
	}

	replaced := patchConn(t, tok, id, mustJSONs(map[string]any{"credentials": map[string]string{"api_key": "synthetic-key-b"},
		"version": 3, "backup_code": backupCode(t, admin)}))
	ok(t, "replace", replaced)
	if replaced.body["last_test"] != nil || replaced.body["enabled"] != true || creds(replaced)["api_key"] == nil {
		t.Fatalf("replace: %s", replaced.raw)
	}
	if v, _ := fake.value(uuid.MustParse(id), "api_key"); v != "synthetic-key-b" {
		t.Fatalf("the emr service holds %q", v)
	}

	// A new path on the same host: the origin is the same, nothing wiped.
	moved := patchConn(t, tok, id, mustJSONs(map[string]any{"base_url": "https://stub.example.org/api/v2", "version": 4,
		"backup_code": backupCode(t, admin)}))
	ok(t, "path", moved)
	if moved.body["enabled"] != true || creds(moved)["api_key"] == nil {
		t.Fatalf("path change: %s", moved.raw)
	}
	for _, c := range fake.callsFor(uuid.MustParse(id)) {
		if c.op == "wipe_credentials" {
			t.Fatal("a same-origin change wiped the credentials")
		}
	}

	got := get(t, connPath(id), tok)
	ok(t, "read", got)
	if version(t, got) != 5 || got.body["updated_by"].(map[string]any)["id"] != admin.id.String() {
		t.Fatalf("read: %s", got.raw)
	}
	var actions []string
	for _, a := range connAudits(t, id) {
		actions = append(actions, a.action)
		if a.actor != admin.id.String() || a.hospital != "" || a.payload["hospital_id"] != hid.String() ||
			a.payload["connection_id"] != id {
			t.Errorf("audit %s: %+v", a.action, a)
		}
	}
	if !slices.Equal(actions, []string{"emr.connection_added", "emr.connection_tested", "emr.connection_switched_on",
		"emr.connection_changed", "emr.connection_changed", "emr.connection_changed"}) {
		t.Fatalf("audit actions %v", actions)
	}
	changes := connAudits(t, id)[3:]
	if mustJSONs(changes[0].payload["fields_changed"]) != `["source_label"]` ||
		mustJSONs(changes[1].payload["fields_changed"]) != `["api_key"]` || changes[1].payload["credentials_replaced"] != true ||
		mustJSONs(changes[2].payload["fields_changed"]) != `["base_url"]` || changes[2].payload["credentials_wiped"] != false {
		t.Fatalf("changes: %+v", changes)
	}
}

// A1: a canary credential sent through POST and PATCH appears in no log
// line, error body, reply, idempotency row, audit payload, database row, or
// NATS message (the boxes are sealed); the reply has set_at only.
func TestCredentialNeverStoredOrLogged(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	canary1 := "synthetic-test-secret-not-real-" + uniq()
	canary2 := "synthetic-test-secret-not-real-" + uniq()
	c := createConn(t, tok, hid, connBody(t, map[string]any{"credentials": map[string]string{"api_key": canary1},
		"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	p := patchConn(t, tok, id, mustJSONs(map[string]any{"credentials": map[string]string{"api_key": canary2}, "version": 1,
		"backup_code": backupCode(t, admin)}))
	ok(t, "patch", p)
	// A refused one too (a bad address): the error body doesn't echo it.
	bad := createConn(t, tok, hid, connBody(t, map[string]any{"base_url": "http://stub.example.org",
		"credentials": map[string]string{"api_key": canary1}, "backup_code": backupCode(t, admin)}))
	problem(t, "bad address", bad, 422, "validation_failed")
	for _, canary := range []string{canary1, canary2} {
		for what, text := range map[string]string{"create reply": c.raw, "patch reply": p.raw, "error body": bad.raw,
			"log": logs.String()} {
			if strings.Contains(text, canary) {
				t.Errorf("%s carries the credential", what)
			}
		}
		var n int
		if err := super.QueryRow(context.Background(), `SELECT
			(SELECT count(*) FROM idempotency_keys WHERE convert_from(COALESCE(body, ''::bytea), 'UTF8') LIKE '%' || $1 || '%' OR request_hash LIKE '%' || $1 || '%')
			+ (SELECT count(*) FROM audit_log WHERE payload::text LIKE '%' || $1 || '%')
			+ (SELECT count(*) FROM emr_connections WHERE row_to_json(emr_connections)::text LIKE '%' || $1 || '%')
			+ (SELECT count(*) FROM emr.credentials WHERE row_to_json(credentials)::text LIKE '%' || $1 || '%')`, canary).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d database rows carry the credential", n)
		}
		fake.mu.Lock()
		for _, m := range fake.seen {
			if bytes.Contains(m, []byte(canary)) {
				t.Error("a NATS message carries the credential in clear")
			}
		}
		fake.mu.Unlock()
	}
	if v, _ := fake.value(uuid.MustParse(id), "api_key"); v != canary2 {
		t.Fatal("the emr service didn't get the credential")
	}
	if set := creds(p)["api_key"]; set == nil {
		t.Fatalf("set_at: %s", p.raw)
	}
}

// The credential routes' idempotency rows hold an HMAC of the request
// under the deployment's key: never the plain SHA-256 a guess could be
// checked against.
func TestIdempotencyHashIsKeyedForCredentialRoutes(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	body := connBody(t, map[string]any{"backup_code": backupCode(t, admin)})
	path := httpx.BasePath + "/platform/hospitals/" + hid.String() + "/emr/connections"
	key := "k-" + uniq()
	r := sendKeyed(t, http.MethodPost, path, tok, body, key)
	if r.code != 200 {
		t.Fatalf("create: %d %s", r.code, r.raw)
	}
	var stored string
	if err := super.QueryRow(context.Background(), `SELECT request_hash FROM idempotency_keys WHERE scope = $1 AND key = $2`,
		admin.id.String(), key).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	for _, part := range []string{http.MethodPost, path, ""} {
		_, _ = io.WriteString(h, strconv.Itoa(len(part))+":"+part+"\n")
	}
	h.Write([]byte(body))
	if stored == hex.EncodeToString(h.Sum(nil)) || len(stored) != 64 {
		t.Fatalf("the stored hash is the plain one (%q)", stored)
	}
	// The same request replays; a changed body with the same key is 409.
	again := sendKeyed(t, http.MethodPost, path, tok, body, key)
	if again.code != 200 || again.body["id"] != r.body["id"] {
		t.Fatalf("replay: %d %s", again.code, again.raw)
	}
	other := sendKeyed(t, http.MethodPost, path, tok, strings.Replace(body, "Main record", "Other record", 1), key)
	problem(t, "same key, other body", other, 409, "conflict")
}

func sendKeyed(t *testing.T, method, path, tok, body, key string) resp {
	t.Helper()
	r, _ := http.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("X-MHAI-Audience", "platform")
	r.Header.Set("Authorization", "Bearer "+tok)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := &recorder{h: http.Header{}}
	handler.ServeHTTP(w, r)
	out := resp{code: w.code, raw: w.buf.String()}
	_ = json.Unmarshal(w.buf.Bytes(), &out.body)
	return out
}

type recorder struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) WriteHeader(c int) {
	if r.code == 0 {
		r.code = c
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.buf.Write(b)
}

// A28, core's side: a PATCH that moves the base address to another host
// wipes the credentials in the emr service before it commits, switches the
// connection off and leaves every set_at null, and is audited
// credentials_wiped; a test then reports sign_in_refused. If the wipe isn't
// confirmed, the PATCH fails and nothing changes.
func TestOriginChangeWipesAndSwitchesOff(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	ok(t, "test", send(t, handler, http.MethodPost, connPath(id)+"/test", tok, ""))
	on := send(t, handler, http.MethodPut, connPath(id)+"/enabled", tok,
		mustJSONs(map[string]any{"enabled": true, "version": 1, "backup_code": backupCode(t, admin)}))
	ok(t, "on", on)

	// The wipe fails: 503, nothing changed.
	fake.wipeFails.Store(true)
	failed := patchConn(t, tok, id, mustJSONs(map[string]any{"base_url": "https://new-host.example.org/api", "version": 2,
		"backup_code": backupCode(t, admin)}))
	fake.wipeFails.Store(false)
	problem(t, "wipe not confirmed", failed, 503, "unavailable")
	now := get(t, connPath(id), tok)
	if now.body["base_url"] != "https://stub.example.org/api" || now.body["enabled"] != true || version(t, now) != 2 ||
		creds(now)["api_key"] == nil {
		t.Fatalf("after a failed wipe: %s", now.raw)
	}
	if n := len(connAudits(t, id)); n != 3 {
		t.Fatalf("a failed PATCH was audited: %d rows", n)
	}

	moved := patchConn(t, tok, id, mustJSONs(map[string]any{"base_url": "https://new-host.example.org/api", "version": 2,
		"backup_code": backupCode(t, admin)}))
	ok(t, "move", moved)
	if moved.body["enabled"] != false || creds(moved)["api_key"] != nil || moved.body["last_test"] != nil {
		t.Fatalf("after the move: %s", moved.raw)
	}
	if _, set := fake.value(uuid.MustParse(id), "api_key"); set {
		t.Fatal("the emr service still holds the credential")
	}
	audits := connAudits(t, id)
	var changed *emrAudit
	for i := range audits {
		if audits[i].action == "emr.connection_changed" {
			changed = &audits[i]
		}
	}
	if changed == nil || changed.payload["credentials_wiped"] != true || audits[len(audits)-1].action != "emr.connection_switched_off" {
		t.Fatalf("audits %+v", audits)
	}
	test := send(t, handler, http.MethodPost, connPath(id)+"/test", tok, "")
	ok(t, "test after move", test)
	if lt := test.body["last_test"].(map[string]any); lt["ok"] != false || lt["code"] != "sign_in_refused" {
		t.Fatalf("test after move: %s", test.raw)
	}
	// Switching on is refused until a test passes.
	problem(t, "on after move", send(t, handler, http.MethodPut, connPath(id)+"/enabled", tok,
		mustJSONs(map[string]any{"enabled": true, "version": version(t, test), "backup_code": backupCode(t, admin)})), 409, "not_tested")
	// New credentials with an origin change are kept for the new origin.
	again := patchConn(t, tok, id, mustJSONs(map[string]any{"base_url": "https://third-host.example.org/api",
		"credentials": map[string]string{"api_key": "synthetic-key-c"}, "version": version(t, test), "backup_code": backupCode(t, admin)}))
	ok(t, "move with credentials", again)
	if creds(again)["api_key"] == nil {
		t.Fatalf("credentials for the new origin: %s", again.raw)
	}
	test = send(t, handler, http.MethodPost, connPath(id)+"/test", tok, "")
	if lt := test.body["last_test"].(map[string]any); lt["ok"] != true {
		t.Fatalf("test with the new credentials: %s", test.raw)
	}
}

// Who may: auditors read and can't write; a clinical account, the clinical
// audience, a session without two-step, and a write without a step-up code
// are refused.
func TestConnectionRoutesRoles(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	aud := auditor(t)
	atok := mint(t, aud)
	ok(t, "auditor list", get(t, "/platform/emr/connections", atok))
	ok(t, "auditor read", get(t, connPath(id), atok))
	ok(t, "auditor types", get(t, "/platform/emr/connector-types", atok))
	problem(t, "auditor create", createConn(t, atok, hid, connBody(t, map[string]any{"backup_code": "x"})), 403, "forbidden")
	problem(t, "auditor patch", patchConn(t, atok, id, `{"version":1}`), 403, "forbidden")
	problem(t, "auditor test", send(t, handler, http.MethodPost, connPath(id)+"/test", atok, ""), 403, "forbidden")
	problem(t, "auditor enable", send(t, handler, http.MethodPut, connPath(id)+"/enabled", atok, `{"enabled":false,"version":1}`), 403, "forbidden")
	// A clinical account on the platform audience.
	clin := seedUser(t, hid, "", authz.RoleClinician)
	problem(t, "clinician list", get(t, "/platform/emr/connections", mint(t, clin)), 403, "not_platform")
	// The clinical audience.
	r := do(t, http.MethodGet, "/platform/emr/connections", tok, false)
	if r.code != 403 {
		t.Errorf("clinical audience: %d %s", r.code, r.raw)
	}
	// No two-step session.
	problem(t, "no two-step", get(t, "/platform/emr/connections", signIn(t, admin, false).tok), 403, "mfa_required")
	// No step-up code; a wrong one.
	problem(t, "create without a code", createConn(t, tok, hid, connBody(t, nil)), 403, "mfa_required")
	problem(t, "patch without a code", patchConn(t, tok, id, `{"version":1,"source_label":"X"}`), 403, "mfa_required")
	problem(t, "enable without a code", send(t, handler, http.MethodPut, connPath(id)+"/enabled", tok, `{"enabled":false,"version":1}`),
		403, "mfa_required")
	problem(t, "wrong code", createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": "wrong-code"})), 403, "mfa_invalid")
	// Nothing was sent to the emr service for any refusal.
	if calls := fake.callsFor(uuid.MustParse(id)); len(calls) != 1 {
		t.Fatalf("calls %+v", calls)
	}
}

// Switching on needs a passing test of the saved settings: none, a failed
// one, or one cleared by an edit is 409 not_tested.
func TestSwitchOnNeedsPassingTestOfSavedSettings(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"credentials": map[string]string{"api_key": "fail"},
		"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	enable := func(v int) resp {
		return send(t, handler, http.MethodPut, connPath(id)+"/enabled", tok,
			mustJSONs(map[string]any{"enabled": true, "version": v, "backup_code": backupCode(t, admin)}))
	}
	problem(t, "never tested", enable(1), 409, "not_tested")
	failed := send(t, handler, http.MethodPost, connPath(id)+"/test", tok, "")
	ok(t, "failing test", failed)
	if lt := failed.body["last_test"].(map[string]any); lt["ok"] != false || lt["code"] != "sign_in_refused" {
		t.Fatalf("failing test: %s", failed.raw)
	}
	problem(t, "failed test", enable(1), 409, "not_tested")
	fixed := patchConn(t, tok, id, mustJSONs(map[string]any{"credentials": map[string]string{"api_key": "synthetic-key-d"},
		"version": 1, "backup_code": backupCode(t, admin)}))
	ok(t, "fix", fixed)
	ok(t, "test", send(t, handler, http.MethodPost, connPath(id)+"/test", tok, ""))
	// An edit of the settings clears the pass.
	edited := patchConn(t, tok, id, mustJSONs(map[string]any{"settings": map[string]string{"client_id": "synthetic-client-2"},
		"version": 2, "backup_code": backupCode(t, admin)}))
	ok(t, "edit", edited)
	problem(t, "edited since the test", enable(3), 409, "not_tested")
	ok(t, "test again", send(t, handler, http.MethodPost, connPath(id)+"/test", tok, ""))
	ok(t, "on", enable(3))
}

// A stale version is 409 conflict, nothing changes; a test whose settings
// changed meanwhile records nothing.
func TestVersionConflict(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	ok(t, "rename", patchConn(t, tok, id, mustJSONs(map[string]any{"source_label": "Second", "version": 1,
		"backup_code": backupCode(t, admin)})))
	problem(t, "stale patch", patchConn(t, tok, id, mustJSONs(map[string]any{"source_label": "Third", "version": 1,
		"backup_code": backupCode(t, admin)})), 409, "conflict")
	problem(t, "stale enable", send(t, handler, http.MethodPut, connPath(id)+"/enabled", tok,
		mustJSONs(map[string]any{"enabled": false, "version": 1, "backup_code": backupCode(t, admin)})), 409, "conflict")
	if got := get(t, connPath(id), tok); got.body["source_label"] != "Second" || version(t, got) != 2 {
		t.Fatalf("after conflicts: %s", got.raw)
	}
}

// Bodies, addresses, types and hospitals that don't fit are refused before
// anything is stored or sent.
func TestConnectionValidation(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	for name, over := range map[string]map[string]any{
		"http address":           {"base_url": "http://stub.example.org"},
		"private address":        {"base_url": "https://10.1.2.3"},
		"metadata address":       {"base_url": "https://169.254.169.254"},
		"localhost":              {"base_url": "https://localhost"},
		"userinfo":               {"base_url": "https://u:p@stub.example.org"},
		"query":                  {"base_url": "https://stub.example.org/?a=1"},
		"long address":           {"base_url": "https://stub.example.org/" + strings.Repeat("p", 480)},
		"empty label":            {"source_label": ""},
		"long label":             {"source_label": strings.Repeat("x", 41)},
		"vendor label":           {"source_label": "Epic records"},
		"control in label":       {"source_label": "Main\nrecord"},
		"missing settings":       {"settings": map[string]string{}},
		"undeclared setting":     {"settings": map[string]string{"client_id": "c", "folder": "/x"}},
		"secret in settings":     {"settings": map[string]string{"client_id": "c", "api_key": "synthetic"}},
		"missing credential":     {"credentials": map[string]string{}},
		"undeclared credential":  {"credentials": map[string]string{"api_key": "a", "private_key": "b"}},
		"long credential":        {"credentials": map[string]string{"api_key": strings.Repeat("k", 4001)}},
		"record link elsewhere":  {"settings": map[string]string{"client_id": "c", "record_link": "https://evil.example.org/{id}"}},
		"ranges without private": {"private_cidrs": []string{"10.9.9.0/24"}},
		"public range":           {"private_network": true, "private_cidrs": []string{"8.8.8.0/24"}},
		"wide range":             {"private_network": true, "private_cidrs": []string{"10.0.0.0/16"}},
		"poll too often":         {"poll_seconds": 30},
		"unknown key":            {"colour": "blue"},
		"version on create":      {"version": 1},
	} {
		over["backup_code"] = backupCode(t, admin)
		problem(t, name, createConn(t, tok, hid, connBody(t, over)), 422, "validation_failed")
	}
	problem(t, "unknown type", createConn(t, tok, hid, connBody(t, map[string]any{"type": "hl7v2_feed",
		"backup_code": backupCode(t, admin)})), 422, "no_type")
	problem(t, "type not offered", createConn(t, tok, hid, connBody(t, map[string]any{"type": "fhir_r4",
		"backup_code": backupCode(t, admin)})), 422, "no_type")
	problem(t, "unknown hospital", createConn(t, tok, uuid.New(), connBody(t, map[string]any{"backup_code": backupCode(t, admin)})),
		404, "not_found")
	removed := seedHospital(t, "removed")
	problem(t, "removed hospital", createConn(t, tok, removed, connBody(t, map[string]any{"backup_code": backupCode(t, admin)})),
		409, "hospital_removed")
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM emr_connections WHERE hospital_id IN ($1, $2)`, hid, removed).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d connections stored (%v)", n, err)
	}
	// Ranges two hospitals declare can't overlap (A37): the second is 422.
	a := createConn(t, tok, hid, connBody(t, map[string]any{"private_network": true, "private_cidrs": []string{"10.88.1.0/24"},
		"backup_code": backupCode(t, admin)}))
	ok(t, "range A", a)
	b := seedHospital(t, "active")
	problem(t, "overlapping range", createConn(t, tok, b, connBody(t, map[string]any{"private_network": true,
		"private_cidrs": []string{"10.88.1.128/25"}, "backup_code": backupCode(t, admin)})), 422, "validation_failed")
	// Unknown connection ids are 404 everywhere.
	for _, r := range []resp{get(t, connPath(uuid.NewString()), tok), get(t, connPath("not-a-uuid"), tok),
		patchConn(t, tok, uuid.NewString(), `{"version":1}`),
		send(t, handler, http.MethodPost, connPath(uuid.NewString())+"/test", tok, "")} {
		problem(t, "unknown connection", r, 404, "not_found")
	}
}

// K4: with EMR off (the default) every write answers 503 not_ready, the
// types list is empty, the list is empty and a connection is 404, even when
// rows exist.
func TestEMRDisabledAnswersNotReady(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create while on", c)
	id := c.body["id"].(string)
	before := len(fake.callsFor(uuid.MustParse(id)))
	types := send(t, emrOff, http.MethodGet, "/platform/emr/connector-types", tok, "")
	if types.code != 200 || mustJSONs(types.body["types"]) != "[]" {
		t.Fatalf("types: %d %s", types.code, types.raw)
	}
	list := send(t, emrOff, http.MethodGet, "/platform/emr/connections", tok, "")
	if list.code != 200 || mustJSONs(list.body) != `{"connections":[],"not_connected":[]}` {
		t.Fatalf("list: %s", list.raw)
	}
	problem(t, "read", send(t, emrOff, http.MethodGet, connPath(id), tok, ""), 404, "not_found")
	for name, r := range map[string]resp{
		"create": send(t, emrOff, http.MethodPost, "/platform/hospitals/"+hid.String()+"/emr/connections", tok,
			connBody(t, map[string]any{"backup_code": backupCode(t, admin)})),
		"patch":  send(t, emrOff, http.MethodPatch, connPath(id), tok, `{"version":1}`),
		"enable": send(t, emrOff, http.MethodPut, connPath(id)+"/enabled", tok, `{"enabled":true,"version":1}`),
		"test":   send(t, emrOff, http.MethodPost, connPath(id)+"/test", tok, ""),
	} {
		problem(t, name, r, 503, "not_ready")
	}
	if after := len(fake.callsFor(uuid.MustParse(id))); after != before {
		t.Fatalf("EMR off sent %d calls", after-before)
	}
}

// Ruling E1: a create whose credentials the emr service doesn't confirm
// stays created, off, with credentials_missing; a retry with the same
// Idempotency-Key gets that connection (same id), never a second one; the
// credentials are re-sent with PATCH on it.
func TestCreateRetryAfterUnconfirmedCredentials(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	path := httpx.BasePath + "/platform/hospitals/" + hid.String() + "/emr/connections"
	body := connBody(t, map[string]any{"backup_code": backupCode(t, admin)})
	key := "k-" + uniq()
	fake.putFails.Store(true)
	first := sendKeyed(t, http.MethodPost, path, tok, body, key)
	fake.putFails.Store(false)
	if first.code != 200 || first.body["enabled"] != false || first.body["credentials_missing"] != true ||
		creds(first)["api_key"] != nil {
		t.Fatalf("unconfirmed create: %d %s", first.code, first.raw)
	}
	again := sendKeyed(t, http.MethodPost, path, tok, body, key)
	if again.code != 200 || again.body["id"] != first.body["id"] {
		t.Fatalf("retry: %d %s", again.code, again.raw)
	}
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM emr_connections WHERE hospital_id = $1`, hid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d connections after the retry (%v)", n, err)
	}
	id := first.body["id"].(string)
	list := get(t, "/platform/emr/connections?hospital="+hid.String(), tok)
	if c := list.body["connections"].([]any)[0].(map[string]any); c["credentials_missing"] != true {
		t.Fatalf("list: %s", list.raw)
	}
	fixed := patchConn(t, tok, id, mustJSONs(map[string]any{"credentials": map[string]string{"api_key": "synthetic-key-e"},
		"version": 1, "backup_code": backupCode(t, admin)}))
	ok(t, "re-send", fixed)
	if fixed.body["credentials_missing"] != false || creds(fixed)["api_key"] == nil || fixed.body["id"] != id {
		t.Fatalf("after re-sending: %s", fixed.raw)
	}
}

// Ruling E1: a create retried without its key (or with another one), naming
// the same label in the same hospital, is 409 conflict: each hospital has a
// label once (in any case). Another hospital may use it; a rename onto a
// taken label is refused too.
func TestCreateSameLabelRefused(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	ok(t, "first", createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)})))
	problem(t, "same label, new key", createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)})),
		409, "conflict")
	problem(t, "same label, other case", createConn(t, tok, hid, connBody(t, map[string]any{"source_label": "MAIN RECORD",
		"backup_code": backupCode(t, admin)})), 409, "conflict")
	r := sendKeyed(t, http.MethodPost, httpx.BasePath+"/platform/hospitals/"+hid.String()+"/emr/connections", tok,
		connBody(t, map[string]any{"backup_code": backupCode(t, admin)}), "")
	problem(t, "same label, no key", r, 409, "conflict")
	var n int
	if err := super.QueryRow(context.Background(), `SELECT count(*) FROM emr_connections WHERE hospital_id = $1`, hid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d connections (%v)", n, err)
	}
	ok(t, "same label, other hospital", createConn(t, tok, seedHospital(t, "active"),
		connBody(t, map[string]any{"backup_code": backupCode(t, admin)})))
	second := createConn(t, tok, hid, connBody(t, map[string]any{"source_label": "Results system", "backup_code": backupCode(t, admin)}))
	ok(t, "second label", second)
	problem(t, "rename onto a taken label", patchConn(t, tok, second.body["id"].(string), mustJSONs(map[string]any{
		"source_label": "Main record", "version": 1, "backup_code": backupCode(t, admin)})), 409, "conflict")
}

// M1: a same-origin credential change is committed, with its audit row,
// before the emr service stores anything: a commit that fails can't leave
// new credentials behind unaudited. If the store then fails, the reply says
// so: saved, switched off, credentials_missing, the old credentials wiped.
func TestSameOriginCredentialChangeCommitsBeforeStoring(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	ok(t, "test", send(t, handler, http.MethodPost, connPath(id)+"/test", tok, ""))
	ok(t, "on", send(t, handler, http.MethodPut, connPath(id)+"/enabled", tok,
		mustJSONs(map[string]any{"enabled": true, "version": 1, "backup_code": backupCode(t, admin)})))

	// When the put arrives, the change and its audit row are already
	// committed (seen from another connection).
	var audited, saved atomic.Bool
	fake.mu.Lock()
	fake.onPut = func(conn string) {
		if conn != id {
			return
		}
		var n, v int
		_ = super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE entity = 'emr_connection'
			AND entity_id = $1 AND action = 'emr.connection_changed' AND payload->>'credentials_replaced' = 'true'`, id).Scan(&n)
		_ = super.QueryRow(context.Background(), `SELECT version FROM emr_connections WHERE id = $1`, id).Scan(&v)
		audited.Store(n == 1)
		saved.Store(v == 3)
	}
	fake.mu.Unlock()
	replaced := patchConn(t, tok, id, mustJSONs(map[string]any{"credentials": map[string]string{"api_key": "synthetic-key-b"},
		"version": 2, "backup_code": backupCode(t, admin)}))
	fake.mu.Lock()
	fake.onPut = nil
	fake.mu.Unlock()
	ok(t, "replace", replaced)
	if !audited.Load() || !saved.Load() {
		t.Fatalf("the credentials were stored before the change was committed (audited %v, saved %v)", audited.Load(), saved.Load())
	}
	if v, _ := fake.value(uuid.MustParse(id), "api_key"); v != "synthetic-key-b" || replaced.body["enabled"] != true ||
		replaced.body["credentials_missing"] != false {
		t.Fatalf("replace: %q %s", v, replaced.raw)
	}

	// The store fails after the commit: 200, the truth about what is saved.
	fake.putFails.Store(true)
	failed := patchConn(t, tok, id, mustJSONs(map[string]any{"credentials": map[string]string{"api_key": "synthetic-key-c"},
		"version": version(t, replaced), "backup_code": backupCode(t, admin)}))
	fake.putFails.Store(false)
	ok(t, "unconfirmed replace", failed)
	if failed.body["credentials_missing"] != true || failed.body["enabled"] != false || creds(failed)["api_key"] != nil {
		t.Fatalf("unconfirmed replace: %s", failed.raw)
	}
	if _, set := fake.value(uuid.MustParse(id), "api_key"); set {
		t.Fatal("the old credentials are still in use after an unconfirmed replace")
	}
	audits := connAudits(t, id)
	if last := audits[len(audits)-1]; last.action != "emr.connection_switched_off" {
		t.Fatalf("audits %+v", audits)
	}
	if got := get(t, connPath(id), tok); got.body["credentials_missing"] != true || version(t, got) != version(t, failed) {
		t.Fatalf("read after: %s", got.raw)
	}
}

// M2: credentials sent with an origin change that the emr service doesn't
// confirm: the change is saved (switched off, wiped), and the reply says so
// with credentials_missing, not 503.
func TestOriginChangeUnconfirmedCredentialsAnswersMissing(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	fake.putFails.Store(true)
	moved := patchConn(t, tok, id, mustJSONs(map[string]any{"base_url": "https://moved-host.example.org/api",
		"credentials": map[string]string{"api_key": "synthetic-key-d"}, "version": 1, "backup_code": backupCode(t, admin)}))
	fake.putFails.Store(false)
	ok(t, "move, credentials unconfirmed", moved)
	if moved.body["base_url"] != "https://moved-host.example.org/api" || moved.body["credentials_missing"] != true ||
		moved.body["enabled"] != false || creds(moved)["api_key"] != nil {
		t.Fatalf("move: %s", moved.raw)
	}
}

// M5: the test route takes no body, an empty object, and nothing else:
// trailing data after {} is refused, before the emr service is asked.
func TestConnectionTestBodyStrict(t *testing.T) {
	admin := platformAdmin(t)
	tok := mint(t, admin)
	hid := seedHospital(t, "active")
	c := createConn(t, tok, hid, connBody(t, map[string]any{"backup_code": backupCode(t, admin)}))
	ok(t, "create", c)
	id := c.body["id"].(string)
	before := len(fake.callsFor(uuid.MustParse(id)))
	for _, body := range []string{"{}junk", "{} {}", `{"a":1}`, "[]", "{", "{}" + strings.Repeat(" ", 2048)} {
		problem(t, "test body "+fmt.Sprintf("%.12q", body), send(t, handler, http.MethodPost, connPath(id)+"/test", tok, body),
			422, "validation_failed")
	}
	if after := len(fake.callsFor(uuid.MustParse(id))); after != before {
		t.Fatalf("a refused body reached the emr service (%d calls)", after-before)
	}
	ok(t, "empty object", send(t, handler, http.MethodPost, connPath(id)+"/test", tok, "{}"))
	ok(t, "whitespace object", send(t, handler, http.MethodPost, connPath(id)+"/test", tok, " { } \n"))
	ok(t, "no body", send(t, handler, http.MethodPost, connPath(id)+"/test", tok, ""))
}
