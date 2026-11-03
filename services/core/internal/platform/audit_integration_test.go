//go:build integration

package platform_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// Plan 4 Task 9: the /platform/audit reads through core's whole chain
// (spec §5.2, §5.3, §5.6; contract §14.9; abuse cases C22, C33). Every
// name, id and string here is synthetic.

const (
	serviceKey = "test-only-bff-service-key-0123456789abcdef"
	testMFAKey = "test-only-mfa-key-0123456789abcdef0123456789"
	marker     = "SYNTHETIC-AUDIT-MARKER-4b9e"
)

var (
	super, app *pgxpool.Pool
	gwNC       *nats.Conn
	buckets    bus.Buckets
	issuer     token.Issuer
	handler    http.Handler
	guard      *authz.Guard
	authSvc    *auth.Service
	// walks receives each run the handler started (Task 10): the walk is
	// held, so runs stay running until a test finishes them.
	walks = make(chan uuid.UUID, 1024)
)

// testContact is the configured security contact (synthetic).
var testContact = platform.SecurityContact{Name: "Synthetic Security Desk", Email: "security@havana.example"}

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
		var jsh jetstream.JetStream
		if gwNC, jsh, err = bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix, bus.TLS{}); err != nil {
			fmt.Fprintln(os.Stderr, "nats:", err)
			return 1
		}
		defer gwNC.Close()
		if buckets, err = bus.OpenBuckets(ctx, jsh, 1); err != nil {
			fmt.Fprintln(os.Stderr, "buckets:", err)
			return 1
		}
		_, priv, _ := ed25519.GenerateKey(rand.Reader)
		issuer = token.Issuer{Key: priv, KeyID: "test-kid"}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		authSvc = &auth.Service{Pool: app, Buckets: buckets, Issuer: issuer, Keys: issuer.PublicKeys(), Log: log,
			MFAKey: []byte(testMFAKey), Argon: password.NewGate(64, time.Minute)}
		svc := authSvc
		guard = &authz.Guard{Keys: issuer.PublicKeys(), Denylist: buckets.Denylist, Pool: app, Log: log,
			Idem: httpx.IdempotentLog(app, log), IdemAnon: httpx.IdempotentAnonLog(app, log)}
		mux := authz.NewMux(guard)
		server.Register(mux, server.Deps{Auth: svc, Pool: app, SecurityContact: testContact,
			AuditWalk: func(id uuid.UUID) { walks <- id }})
		handler = mux.Handler([]string{serviceKey})
		return m.Run()
	}()
	os.Exit(code)
}

func uniq() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

func seedHospital(t *testing.T, status string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ($1, $2, $3) RETURNING id`,
		"Synthetic General "+uniq(), "H"+uniq(), status).Scan(&id); err != nil {
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
	email   string
}

// seedUser adds a user of hospital hid (uuid.Nil: a platform account),
// named name ("" for a generated name).
func seedUser(t *testing.T, hid uuid.UUID, name string, roles ...string) user {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	var h any
	if hid != uuid.Nil {
		h = hid
	}
	if name == "" {
		name = "Dr Synthetic " + uniq()
	}
	email := "synthetic." + uniq() + "@havana.example"
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, full_name)
		VALUES ($1, $2, 'synthetic-hash', $3, 'Synthetic Fullname') RETURNING id`, h, email, name).Scan(&id); err != nil {
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
	return user{id: id, hid: hid, roles: roles, name: name, email: email}
}

func auditor(t *testing.T) user { return seedUser(t, uuid.Nil, "", authz.RoleAuditor) }

// session is a signed-in session: its token and id.
type session struct {
	tok string
	sid uuid.UUID
}

func signIn(t *testing.T, u user, mfa bool) session {
	t.Helper()
	sid := uuid.New()
	amr := []string{"pwd"}
	if mfa {
		amr = append(amr, "otp")
	}
	if _, err := super.Exec(context.Background(), `INSERT INTO sessions (id, user_id, mfa, amr) VALUES ($1, $2, $3, $4)`,
		sid, u.id, mfa, amr); err != nil {
		t.Fatal(err)
	}
	hid := ""
	if u.hid != uuid.Nil {
		hid = u.hid.String()
	}
	at, _, err := issuer.Access(token.Claims{Sub: u.id.String(), Hid: hid, Roles: u.roles, MFA: mfa, AMR: amr,
		Sid: sid.String(), Jti: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return session{tok: at, sid: sid}
}

func mint(t *testing.T, u user) string { return signIn(t, u, true).tok }

type resp struct {
	code int
	body map[string]any
	raw  string
}

func do(t *testing.T, method, path, tok string, platform bool) resp {
	t.Helper()
	r := httptest.NewRequest(method, httpx.BasePath+path, nil)
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("X-MHAI-Client-IP", "198.51.100.9")
	if platform {
		r.Header.Set("X-MHAI-Audience", "platform")
	}
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	if method != http.MethodGet {
		r.Header.Set("Idempotency-Key", "k-"+uniq())
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	out := resp{code: w.Code, raw: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.body)
	return out
}

func get(t *testing.T, path, tok string) resp { return do(t, http.MethodGet, path, tok, true) }

func problem(t *testing.T, what string, r resp, status int, code string) {
	t.Helper()
	if r.code != status || r.body["code"] != code {
		t.Errorf("%s: want %d %s, got %d %s", what, status, code, r.code, r.raw)
	}
}

func ok(t *testing.T, what string, r resp) {
	t.Helper()
	if r.code != 200 {
		t.Fatalf("%s: %d %s", what, r.code, r.raw)
	}
}

// row is a chain row as the tests seed it: written straight into audit_log
// as the superuser (the reader never checks hashes), so a test can set the
// time, and seed payloads no schema would let through.
type row struct {
	at                  time.Time
	actor, hospital     uuid.UUID
	rid, action, entity string
	entityID            string
	payload             map[string]any
}

func randHex() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func insertRow(t *testing.T, r row) int64 {
	t.Helper()
	if r.at.IsZero() {
		r.at = time.Now().UTC()
	}
	if r.payload == nil {
		r.payload = map[string]any{}
	}
	raw, err := json.Marshal(r.payload)
	if err != nil {
		t.Fatal(err)
	}
	var actor, hospital any
	if r.actor != uuid.Nil {
		actor = r.actor
	}
	if r.hospital != uuid.Nil {
		hospital = r.hospital
	}
	var id int64
	if err := super.QueryRow(context.Background(), `
		INSERT INTO audit_log (ts, actor_id, hospital_id, request_id, action, entity, entity_id, payload, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10) RETURNING id`,
		r.at, actor, hospital, nullable(r.rid), r.action, nullable(r.entity), nullable(r.entityID), string(raw),
		randHex(), randHex()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func events(t *testing.T, r resp) []map[string]any {
	t.Helper()
	list, okList := r.body["events"].([]any)
	if !okList {
		t.Fatalf("no events in %s", r.raw)
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, isMap := e.(map[string]any)
		if !isMap {
			t.Fatalf("event %v is not an object", e)
		}
		out = append(out, m)
	}
	return out
}

func seqs(t *testing.T, r resp) []int64 {
	t.Helper()
	var out []int64
	for _, e := range events(t, r) {
		out = append(out, int64(e["seq"].(float64)))
	}
	return out
}

func keysOf(m map[string]any) []string {
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
	if got := keysOf(m); !slices.Equal(got, want) {
		t.Errorf("%s keys %v, want %v", what, got, want)
	}
}

func desc(ids ...int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

type auditRow struct {
	action, actor, entity, entityID string
	hospital                        *string
	payload                         map[string]any
}

// readsBy are the audit.viewed and audit.searched rows u wrote.
func readsBy(t *testing.T, u user) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, actor_id::text, COALESCE(entity, ''), COALESCE(entity_id, ''),
		hospital_id::text, payload FROM audit_log WHERE actor_id = $1 AND action IN ('audit.viewed', 'audit.searched') ORDER BY id`, u.id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var raw []byte
		if err := rows.Scan(&a.action, &a.actor, &a.entity, &a.entityID, &a.hospital, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &a.payload); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// actionsBy are u's rows of one action.
func actionsBy(t *testing.T, u user, action string) []auditRow {
	t.Helper()
	var out []auditRow
	for _, r := range readsBy(t, u) {
		if r.action == action {
			out = append(out, r)
		}
	}
	return out
}

var eventKeys = []string{"id", "seq", "hash", "at", "actor", "hospital", "type", "kind", "target", "outcome",
	"ip", "device", "flag", "reason", "note", "refs", "facts", "after_break"}

var bareKeys = []string{"id", "seq", "hash", "at", "type", "kind"}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// --- the list's shape --------------------------------------------------------

func TestAuditListShapeAndOrder(t *testing.T) {
	h := seedHospital(t, "active")
	c := seedUser(t, h, "", authz.RoleClinician)
	admin := seedUser(t, h, "", authz.RoleHospitalAdmin, authz.RoleAnalyst)
	a := auditor(t)
	tok := mint(t, a)
	sessionID := uuid.NewString()
	corr := uuid.NewString()
	rid := "0123456789abcdef0123456789abcdef"
	signedIn := insertRow(t, row{actor: c.id, hospital: h, rid: rid, action: "auth.login.succeeded",
		payload: map[string]any{"mfa": true}})
	loggedOut := insertRow(t, row{actor: admin.id, hospital: h, action: "auth.logout", entity: "session", entityID: sessionID})
	approved := insertRow(t, row{actor: c.id, hospital: h, action: "correction.approved",
		payload: map[string]any{"correction_id": corr, "job_id": uuid.NewString()}})
	withheld := insertRow(t, row{actor: c.id, hospital: h, rid: rid, action: "auth.mfa.verified",
		payload: map[string]any{"method": "otp"}})
	system := insertRow(t, row{hospital: h, action: "correction.applied",
		payload: map[string]any{"job_id": uuid.NewString(), "correction_id": corr}})

	r := get(t, "/platform/audit?hospital="+h.String(), tok)
	ok(t, "list", r)
	sameKeys(t, "envelope", r.body, "events", "next_cursor", "total", "total_capped", "oldest_kept_at", "last_checkpoint")
	if got, want := seqs(t, r), desc(signedIn, loggedOut, approved, withheld, system); !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v (newest first)", got, want)
	}
	if r.body["next_cursor"] != nil || r.body["total"] != float64(5) || r.body["total_capped"] != false ||
		r.body["last_checkpoint"] != nil {
		t.Errorf("envelope %s", r.raw)
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(r.body["oldest_kept_at"])); err != nil {
		t.Errorf("oldest_kept_at %v", r.body["oldest_kept_at"])
	}
	bySeq := map[int64]map[string]any{}
	for _, e := range events(t, r) {
		seq := int64(e["seq"].(float64))
		bySeq[seq] = e
		if e["id"] != strconv.FormatInt(seq, 10) || !hexHash.MatchString(fmt.Sprint(e["hash"])) {
			t.Errorf("event %d: id %v hash %v", seq, e["id"], e["hash"])
		}
		if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(e["at"])); err != nil {
			t.Errorf("event %d: at %v", seq, e["at"])
		}
	}
	// A withheld row is the bare entry.
	sameKeys(t, "withheld entry", bySeq[withheld], bareKeys...)
	if bySeq[withheld]["type"] != "security" || bySeq[withheld]["kind"] != "unknown" {
		t.Errorf("withheld entry %v", bySeq[withheld])
	}
	// The mapped rows have exactly the contract's keys.
	e := bySeq[signedIn]
	sameKeys(t, "event", e, eventKeys...)
	actor := e["actor"].(map[string]any)
	sameKeys(t, "actor", actor, "id", "name", "role", "kind")
	if actor["id"] != c.id.String() || actor["name"] != c.name || actor["role"] != "clinician" || actor["kind"] != "staff" {
		t.Errorf("actor %v", actor)
	}
	hosp := e["hospital"].(map[string]any)
	sameKeys(t, "hospital", hosp, "id", "name", "removed")
	if hosp["id"] != h.String() || hosp["removed"] != false {
		t.Errorf("hospital %v", hosp)
	}
	if e["type"] != "sign_in" || e["kind"] != "signed_in" || e["outcome"] != "succeeded" || e["target"] != nil ||
		e["ip"] != nil || e["device"] != nil || e["flag"] != nil || e["reason"] != nil || e["note"] != nil ||
		e["after_break"] != false {
		t.Errorf("signed_in event %v", e)
	}
	refs := e["refs"].(map[string]any)
	sameKeys(t, "refs", refs, "clearance_id", "export_id", "request_id")
	if refs["request_id"] != rid || refs["clearance_id"] != nil || refs["export_id"] != nil {
		t.Errorf("refs %v", refs)
	}
	if f, isMap := e["facts"].(map[string]any); !isMap || len(f) != 0 {
		t.Errorf("facts %v", e["facts"])
	}
	// A hospital admin who is also an analyst is a hospital_admin; the
	// session target is the row's entity.
	lo := bySeq[loggedOut]
	if lo["actor"].(map[string]any)["role"] != "hospital_admin" {
		t.Errorf("hospital admin actor %v", lo["actor"])
	}
	if tg, _ := lo["target"].(map[string]any); tg["kind"] != "session" || tg["id"] != sessionID {
		t.Errorf("signed_out target %v", lo["target"])
	}
	// A clinician deciding a correction acts as a reviewer.
	if bySeq[approved]["actor"].(map[string]any)["role"] != "reviewer" || bySeq[approved]["kind"] != "review_submitted" {
		t.Errorf("approval %v", bySeq[approved])
	}
	// A row with no actor is the system's.
	sys := bySeq[system]["actor"].(map[string]any)
	if sys["id"] != "system" || sys["name"] != "System" || sys["role"] != "system" || sys["kind"] != "system" {
		t.Errorf("system actor %v", sys)
	}

	// The detail: the event, with patients and letter null.
	d := get(t, "/platform/audit/"+strconv.FormatInt(signedIn, 10), tok)
	ok(t, "detail", d)
	sameKeys(t, "detail", d.body, append(slices.Clone(eventKeys), "patients", "letter")...)
	if d.body["patients"] != nil || d.body["letter"] != nil || d.body["kind"] != "signed_in" {
		t.Errorf("detail %s", d.raw)
	}
	d = get(t, "/platform/audit/"+strconv.FormatInt(withheld, 10), tok)
	ok(t, "withheld detail", d)
	sameKeys(t, "withheld detail", d.body, append(slices.Clone(bareKeys), "patients", "letter")...)
	for _, bad := range []string{"999999999", "0", "-1", "01", "abc", "1e3", "99999999999999999999"} {
		problem(t, "detail "+bad, get(t, "/platform/audit/"+bad, tok), 404, "not_found")
	}

	// The hospitals the log names, removed ones included, and none it doesn't.
	removed := seedHospital(t, "removed")
	insertRow(t, row{hospital: removed, action: "auth.logout"})
	quiet := seedHospital(t, "active")
	hr := get(t, "/platform/audit/hospitals", tok)
	ok(t, "hospitals", hr)
	sameKeys(t, "hospitals reply", hr.body, "hospitals")
	seen := map[string]map[string]any{}
	for _, x := range hr.body["hospitals"].([]any) {
		m := x.(map[string]any)
		sameKeys(t, "hospital", m, "id", "name", "removed")
		seen[m["id"].(string)] = m
	}
	if seen[h.String()] == nil || seen[h.String()]["removed"] != false {
		t.Errorf("hospital %s missing or wrong: %v", h, seen[h.String()])
	}
	if seen[removed.String()] == nil || seen[removed.String()]["removed"] != true {
		t.Errorf("removed hospital missing or not removed: %v", seen[removed.String()])
	}
	if seen[quiet.String()] != nil {
		t.Errorf("a hospital the log doesn't name is listed")
	}
}

// --- filters -----------------------------------------------------------------

func TestAuditFilters(t *testing.T) {
	h := seedHospital(t, "active")
	other := seedHospital(t, "active")
	u1 := seedUser(t, h, "", authz.RoleClinician)
	u2 := seedUser(t, h, "", authz.RoleClinician)
	p := auditor(t)
	tok := mint(t, auditor(t))
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	a := insertRow(t, row{at: at(0), actor: u1.id, hospital: h, action: "auth.login.succeeded"})
	b := insertRow(t, row{at: at(1), actor: u2.id, hospital: h, action: "auth.login.failed",
		payload: map[string]any{"reason": "bad_credentials"}})
	c := insertRow(t, row{at: at(2), actor: u1.id, hospital: h, action: "auth.mfa.locked",
		payload: map[string]any{"reason": "token_attempts"}})
	d := insertRow(t, row{at: at(3), actor: u2.id, hospital: h, action: "correction.apply_failed",
		payload: map[string]any{"job_id": uuid.NewString(), "error": "internal_error"}})
	e := insertRow(t, row{at: at(4), actor: u1.id, hospital: h, action: "correction.decision_refused",
		payload: map[string]any{"correction_id": uuid.NewString(), "reason": "self_approval", "attempted": "approve"}})
	f := insertRow(t, row{at: at(5), actor: u2.id, hospital: h, action: "auth.mfa.verified"})
	g := insertRow(t, row{at: at(6), actor: u1.id, hospital: h, action: "audit.verify_finished",
		payload: map[string]any{"run_id": uuid.NewString(), "state": "broken", "first_broken_seq": 3}})
	elsewhere := insertRow(t, row{at: at(2), actor: u1.id, hospital: other, action: "auth.login.succeeded"})
	plat := insertRow(t, row{at: at(7), actor: p.id, action: "audit.searched",
		payload: map[string]any{"searched_kind": "event"}})

	hq := "hospital=" + h.String()
	for _, c := range []struct {
		query string
		want  []int64
	}{
		{hq, desc(a, b, c, d, e, f, g)},
		{hq + "&type=sign_in", desc(a, b)},
		{hq + "&type=sign_in,review", desc(a, b, d)},
		{hq + "&type=review,sign_in,review", desc(a, b, d)},
		{hq + "&type=security", desc(c, e, f)},
		{hq + "&type=audit", desc(g)},
		{hq + "&type=export", nil},
		{hq + "&outcome=refused", desc(b, c, e)},
		{hq + "&outcome=failed", desc(d)},
		{hq + "&outcome=succeeded", desc(a, g)},
		{hq + "&flagged=true", desc(c, e, g)},
		{hq + "&flagged=false", desc(a, b, d, f)},
		{hq + "&person=" + u1.id.String(), desc(a, c, e, g)},
		{hq + "&person=" + u1.id.String() + "&outcome=refused", desc(c, e)},
		{hq + "&person=" + u2.id.String() + "&type=sign_in,security", desc(b, f)},
		{hq + "&type=security&flagged=true", desc(c, e)},
		{hq + "&from=" + url.QueryEscape(at(2).Format(time.RFC3339)) + "&to=" + url.QueryEscape(at(5).Format(time.RFC3339)), desc(c, d, e)},
		{hq + "&from=" + url.QueryEscape(at(2).In(time.FixedZone("x", 3600)).Format(time.RFC3339)), desc(c, d, e, f, g)},
		{hq + "&to=" + url.QueryEscape(at(1).Format(time.RFC3339Nano)), desc(a)},
		{"person=" + u1.id.String() + "&from=" + url.QueryEscape(at(2).Format(time.RFC3339)) + "&to=" + url.QueryEscape(at(3).Format(time.RFC3339)), desc(c, elsewhere)},
		{"hospital=platform&person=" + p.id.String(), desc(plat)},
		{"hospital=platform&person=" + u1.id.String(), nil},
	} {
		r := get(t, "/platform/audit?"+c.query, tok)
		ok(t, c.query, r)
		if got := seqs(t, r); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.query, got, c.want)
		}
		if r.body["total"] != float64(len(c.want)) {
			t.Errorf("%s: total %v, want %d", c.query, r.body["total"], len(c.want))
		}
	}
	// Each returned event agrees with the filter that found it.
	r := get(t, "/platform/audit?"+hq+"&outcome=refused", tok)
	for _, ev := range events(t, r) {
		if ev["outcome"] != "refused" {
			t.Errorf("outcome=refused returned %v", ev)
		}
	}

	for _, bad := range []string{
		"type=nope", "type=", "type=sign_in,", "outcome=maybe", "flagged=yes", "flagged=1",
		"hospital=not-a-uuid", "hospital=" + strings.ToUpper(h.String()), "person=P-ABCD12", "person=",
		"from=yesterday", "from=2026-10-01T12:00:00", "to=2026-13-01T00:00:00Z",
		"limit=0", "limit=101", "limit=x", "limit=-1", "colour=blue",
		hq + "&" + hq, "type=audit&type=review", "cursor=",
	} {
		problem(t, bad, get(t, "/platform/audit?"+bad, tok), 422, "validation_failed")
	}
	// limit is 1 to 100.
	r = get(t, "/platform/audit?"+hq+"&limit=1", tok)
	ok(t, "limit=1", r)
	if got := seqs(t, r); !slices.Equal(got, []int64{g}) || r.body["next_cursor"] == nil {
		t.Errorf("limit=1: %v %v", got, r.body["next_cursor"])
	}
	ok(t, "limit=100", get(t, "/platform/audit?"+hq+"&limit=100", tok))
}

// C33 (the audit part): a cursor is bound to the filters it was made under.
func TestAuditCursorBoundToFilters(t *testing.T) {
	h := seedHospital(t, "active")
	tok := mint(t, auditor(t))
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, insertRow(t, row{hospital: h, action: "auth.logout"}))
	}
	hq := "hospital=" + h.String()
	var got []int64
	var cursor string
	for page := 0; ; page++ {
		path := "/platform/audit?" + hq + "&limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := get(t, path, tok)
		ok(t, "page", r)
		got = append(got, seqs(t, r)...)
		if r.body["total"] != float64(5) {
			t.Errorf("page %d total %v", page, r.body["total"])
		}
		if r.body["next_cursor"] == nil {
			break
		}
		cursor = r.body["next_cursor"].(string)
		if page > 5 {
			t.Fatal("paging never ends")
		}
	}
	if !slices.Equal(got, desc(ids...)) {
		t.Fatalf("pages %v, want %v", got, desc(ids...))
	}
	r := get(t, "/platform/audit?"+hq+"&limit=2", tok)
	cursor = r.body["next_cursor"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{48}$`).MatchString(cursor) {
		t.Errorf("cursor %q is not 48 lowercase hex", cursor)
	}
	// The same filters, given in another order, take it.
	ok(t, "same filters", get(t, "/platform/audit?limit=2&cursor="+url.QueryEscape(cursor)+"&"+hq, tok))
	other := seedHospital(t, "active")
	for _, q := range []string{
		"hospital=" + other.String(), hq + "&type=sign_in", hq + "&outcome=succeeded", hq + "&flagged=true",
		"", hq + "&from=2026-01-01T00:00:00Z", "q=" + strconv.FormatInt(ids[0], 10),
	} {
		problem(t, "cursor under "+q, get(t, "/platform/audit?"+q+"&cursor="+url.QueryEscape(cursor), tok), 422, "validation_failed")
	}
	for _, bad := range []string{"garbage", "AAAA", cursor + "A", cursor[:len(cursor)-2], strings.ToUpper(cursor)} {
		problem(t, "cursor "+bad, get(t, "/platform/audit?"+hq+"&cursor="+url.QueryEscape(bad), tok), 422, "validation_failed")
	}
}

// The cursor is HMAC-bound to its session: a forged cursor, or another
// session's, is refused, so no page is read through a cursor that wasn't
// handed out, and every page, cursor or not, is recorded (security review).
func TestAuditForgedCursorRefusedAndReadsRecorded(t *testing.T) {
	h := seedHospital(t, "active")
	for i := 0; i < 4; i++ {
		insertRow(t, row{hospital: h, action: "auth.logout"})
	}
	hq := "hospital=" + h.String()
	a := auditor(t)
	s1 := signIn(t, a, true)
	r := get(t, "/platform/audit?"+hq+"&limit=1", s1.tok)
	ok(t, "first page", r)
	mine := r.body["next_cursor"].(string)
	if n := len(readsBy(t, a)); n != 1 {
		t.Fatalf("first page: want 1 row, got %d", n)
	}
	// The newest page through a forged cursor: last = MaxInt64 with the
	// unkeyed filter hash, a zero tag, or this session's tag; and the
	// unkeyed base64url form the first version made.
	maxID := "7fffffffffffffff"
	sum := sha256.Sum256([]byte(`{"hospital":"` + h.String() + `"}`))
	forged := []string{
		maxID + hex.EncodeToString(sum[:16]),
		maxID + strings.Repeat("0", 32),
		maxID + mine[16:],
		base64.RawURLEncoding.EncodeToString(append([]byte{0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, sum[:16]...)),
	}
	s2 := signIn(t, a, true)
	other := signIn(t, auditor(t), true)
	for _, c := range forged {
		for _, s := range []session{s1, s2, other} {
			problem(t, "forged cursor "+c, get(t, "/platform/audit?"+hq+"&cursor="+url.QueryEscape(c), s.tok), 422, "validation_failed")
		}
	}
	// Another session's cursor, even the same user's.
	problem(t, "same user, other session", get(t, "/platform/audit?"+hq+"&limit=1&cursor="+url.QueryEscape(mine), s2.tok), 422, "validation_failed")
	problem(t, "other user", get(t, "/platform/audit?"+hq+"&limit=1&cursor="+url.QueryEscape(mine), other.tok), 422, "validation_failed")
	if n := len(readsBy(t, a)); n != 1 {
		t.Fatalf("refused cursors wrote rows: %d", n)
	}
	// A real cursor page after the page load's 5 minutes is recorded: a
	// cursor never decides whether a read is recorded.
	if _, err := super.Exec(context.Background(),
		`UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE scope = 'audit.read'`); err != nil {
		t.Fatal(err)
	}
	ok(t, "cursor page after expiry", get(t, "/platform/audit?"+hq+"&limit=1&cursor="+url.QueryEscape(mine), s1.tok))
	reads := readsBy(t, a)
	if len(reads) != 2 || reads[1].action != "audit.viewed" {
		t.Fatalf("cursor page after expiry: want a second audit.viewed, got %+v", reads)
	}
}

func TestAuditTotalCapped(t *testing.T) {
	big := seedHospital(t, "active")
	small := seedHospital(t, "active")
	if _, err := super.Exec(context.Background(), `
		INSERT INTO audit_log (ts, hospital_id, action, payload, prev_hash, hash)
		SELECT now(), $1, 'auth.logout', '{}', md5(g::text || 'p') || md5(g::text || 'q'), md5(g::text || $2) || md5($2 || g::text)
		FROM generate_series(1, 10003) g`, big, uniq()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		insertRow(t, row{hospital: small, action: "auth.logout"})
	}
	// The person filter's page query uses audit_log_actor_idx, not a scan
	// of the 10k-row table.
	ctx := context.Background()
	if _, err := super.Exec(ctx, `ANALYZE audit_log`); err != nil {
		t.Fatal(err)
	}
	rows, err := super.Query(ctx, `EXPLAIN SELECT id FROM audit_log WHERE actor_id = $1 ORDER BY id DESC LIMIT 51`, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	if p := strings.Join(plan, "\n"); !strings.Contains(p, "audit_log_actor_idx") || strings.Contains(p, "Seq Scan") {
		t.Errorf("person filter plan doesn't use audit_log_actor_idx:\n%s", p)
	}
	rows, err = super.Query(ctx, `EXPLAIN SELECT id FROM audit_log WHERE request_id = $1 ORDER BY id DESC LIMIT 51`, "req_"+uniq())
	if err != nil {
		t.Fatal(err)
	}
	plan = nil
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	if p := strings.Join(plan, "\n"); !strings.Contains(p, "audit_log_request_idx") || strings.Contains(p, "Seq Scan") {
		t.Errorf("request search plan doesn't use audit_log_request_idx:\n%s", p)
	}
	tok := mint(t, auditor(t))
	r := get(t, "/platform/audit?hospital="+big.String(), tok)
	ok(t, "big", r)
	if r.body["total"] != float64(10000) || r.body["total_capped"] != true || len(events(t, r)) != 50 {
		t.Errorf("big: total %v capped %v events %d", r.body["total"], r.body["total_capped"], len(events(t, r)))
	}
	r = get(t, "/platform/audit?hospital="+small.String(), tok)
	ok(t, "small", r)
	if r.body["total"] != float64(3) || r.body["total_capped"] != false {
		t.Errorf("small: total %v capped %v", r.body["total"], r.body["total_capped"])
	}
}

// --- reads are recorded --------------------------------------------------------

func TestAuditViewedOncePerPageLoad(t *testing.T) {
	h := seedHospital(t, "active")
	for i := 0; i < 3; i++ {
		insertRow(t, row{hospital: h, action: "auth.logout"})
	}
	a := auditor(t)
	s := signIn(t, a, true)
	hq := "hospital=" + h.String()
	r := get(t, "/platform/audit?"+hq+"&limit=1", s.tok)
	ok(t, "first", r)
	ok(t, "again", get(t, "/platform/audit?"+hq+"&limit=1", s.tok))
	ok(t, "again, another limit", get(t, "/platform/audit?"+hq+"&limit=2", s.tok))
	ok(t, "cursor", get(t, "/platform/audit?"+hq+"&limit=1&cursor="+url.QueryEscape(r.body["next_cursor"].(string)), s.tok))
	reads := readsBy(t, a)
	if len(reads) != 1 {
		t.Fatalf("want one audit.viewed, got %+v", reads)
	}
	v := reads[0]
	if v.action != "audit.viewed" || v.hospital != nil || v.entity != "" || v.entityID != "" {
		t.Errorf("viewed row %+v", v)
	}
	want := map[string]any{"list": "events", "filters": map[string]any{"hospital": h.String()}}
	if got, _ := json.Marshal(v.payload); string(got) != mustJSON(want) {
		t.Errorf("payload %s, want %s", got, mustJSON(want))
	}
	// Other filters are another read.
	ok(t, "other filters", get(t, "/platform/audit?"+hq+"&type=sign_in,audit&flagged=true", s.tok))
	reads = readsBy(t, a)
	if len(reads) != 2 {
		t.Fatalf("want two audit.viewed, got %d", len(reads))
	}
	wantF := map[string]any{"hospital": h.String(), "type": []any{"sign_in", "audit"}, "flagged": true}
	if got, _ := json.Marshal(reads[1].payload["filters"]); string(got) != mustJSON(wantF) {
		t.Errorf("filters %s, want %s", got, mustJSON(wantF))
	}
	// Another session is another page load.
	ok(t, "other session", get(t, "/platform/audit?"+hq, signIn(t, a, true).tok))
	if n := len(readsBy(t, a)); n != 3 {
		t.Fatalf("another session: want 3 rows, got %d", n)
	}
	// After 5 minutes the same read is recorded again.
	if _, err := super.Exec(context.Background(),
		`UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE scope = 'audit.read'`); err != nil {
		t.Fatal(err)
	}
	ok(t, "after expiry", get(t, "/platform/audit?"+hq+"&limit=1", s.tok))
	if n := len(readsBy(t, a)); n != 4 {
		t.Fatalf("after expiry: want 4 rows, got %d", n)
	}
	// The hospitals and people reads write no audit.viewed (they are
	// searches: TestAuditPeopleAndHospitalsRecorded).
	ok(t, "hospitals", get(t, "/platform/audit/hospitals", s.tok))
	ok(t, "people", get(t, "/platform/audit/people?q=Synthetic", s.tok))
	if n := len(actionsBy(t, a, "audit.viewed")); n != 4 {
		t.Fatalf("other reads wrote audit.viewed rows: %d", n)
	}
	// Parallel first pages of one new filter set: one row.
	b := auditor(t)
	sb := signIn(t, b, true)
	done := make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() {
			done <- get(t, "/platform/audit?"+hq+"&outcome=succeeded", sb.tok).code
		}()
	}
	for i := 0; i < 8; i++ {
		if c := <-done; c != 200 {
			t.Errorf("parallel read: %d", c)
		}
	}
	if n := len(readsBy(t, b)); n != 1 {
		t.Errorf("parallel first pages: want 1 row, got %d", n)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAuditSearchIgnoresFiltersAndAudits(t *testing.T) {
	h := seedHospital(t, "active")
	other := seedHospital(t, "active")
	rid := "req_" + uniq() + uniq()
	x := insertRow(t, row{hospital: h, rid: rid, action: "auth.logout"})
	y := insertRow(t, row{hospital: h, rid: rid, action: "auth.login.succeeded"})
	insertRow(t, row{hospital: h, rid: "req_" + uniq() + uniq(), action: "auth.logout"})
	a := auditor(t)
	s := signIn(t, a, true)

	// An event id, with filters that would exclude it: they're ignored.
	r := get(t, "/platform/audit?q="+strconv.FormatInt(x, 10)+"&hospital="+other.String()+"&type=audit&outcome=failed", s.tok)
	ok(t, "event search", r)
	if got := seqs(t, r); !slices.Equal(got, []int64{x}) || r.body["total"] != float64(1) {
		t.Errorf("event search: %v total %v", got, r.body["total"])
	}
	// A request id: every row of the request, newest first.
	r = get(t, "/platform/audit?q="+rid+"&person="+uuid.NewString(), s.tok)
	ok(t, "request search", r)
	if got := seqs(t, r); !slices.Equal(got, desc(x, y)) {
		t.Errorf("request search: %v", got)
	}
	// Paging a request search keeps q.
	r = get(t, "/platform/audit?q="+rid+"&limit=1", s.tok)
	ok(t, "request search page", r)
	c, _ := r.body["next_cursor"].(string)
	r2 := get(t, "/platform/audit?q="+rid+"&limit=1&cursor="+url.QueryEscape(c), s.tok)
	ok(t, "request search page 2", r2)
	if got := seqs(t, r2); !slices.Equal(got, []int64{x}) {
		t.Errorf("page 2: %v", got)
	}
	// An identical repeat within 5 minutes is the same read.
	ok(t, "repeat", get(t, "/platform/audit?q="+strconv.FormatInt(x, 10), s.tok))
	// Nothing found is an empty list.
	r = get(t, "/platform/audit?q=987654321012", s.tok)
	ok(t, "no match", r)
	if len(events(t, r)) != 0 || r.body["total"] != float64(0) {
		t.Errorf("no match: %s", r.raw)
	}

	reads := readsBy(t, a)
	var kinds []string
	for _, rr := range reads {
		if rr.action != "audit.searched" {
			t.Errorf("a search wrote %s", rr.action)
			continue
		}
		if rr.hospital != nil || len(rr.payload) != 1 {
			t.Errorf("searched row %+v", rr)
		}
		kinds = append(kinds, fmt.Sprint(rr.payload["searched_kind"])+":"+rr.entity+":"+rr.entityID)
	}
	want := []string{
		"event:audit_event:" + strconv.FormatInt(x, 10),
		"request:request:" + rid,
		"event:audit_event:987654321012",
	}
	if !slices.Equal(kinds, want) {
		t.Errorf("searched rows %v, want %v", kinds, want)
	}

	// q is an exact id: a pseudonym, a short or free-text value is refused
	// and nothing is recorded.
	for _, bad := range []string{"P-ABCD1234", "P-7F3K2XYZ", "short", "has space here", "0", "007", "a/b/c/d/e/f",
		strings.Repeat("a", 65), "SYNTHETIC " + marker} {
		problem(t, "q="+bad, get(t, "/platform/audit?q="+url.QueryEscape(bad), s.tok), 422, "validation_failed")
	}
	if n := len(readsBy(t, a)); n != len(want) {
		t.Errorf("a refused search was recorded: %d rows", n)
	}
}

// --- C22: nothing free-text or pseudonym-shaped leaves -------------------------

var pseudonymIn = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)

func TestAuditResponsesHaveNoPseudonymOrFreeText(t *testing.T) {
	h := seedHospital(t, "active")
	c := seedUser(t, h, "", authz.RoleClinician)
	pseudo := seedUser(t, h, "Dr P-ABCD12", authz.RoleClinician)
	tok := mint(t, auditor(t))
	pseudonyms := []string{"P-ABCD12", "P-7F3K2", "x P-ABCD1 y", "req-P-7F3K2"}
	actions := []string{"test.made_up.action"}
	for a := range audit.Catalogue {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for i, a := range actions {
		p := pseudonyms[i%len(pseudonyms)]
		// Marker keys and values the schema doesn't take.
		insertRow(t, row{actor: c.id, hospital: h, rid: marker, action: a, entity: "session", entityID: p,
			payload: map[string]any{"note": marker, "reason": marker, "state": p, "list": marker,
				"filters": map[string]any{"q": p, "hospital": marker}, "searched": p, "run_id": p,
				"first_broken_seq": p, "searched_kind": marker, "outcome_note": marker}})
		// A pseudonym-shaped request id and entity on a payload that fits.
		insertRow(t, row{actor: c.id, hospital: h, rid: "P-7F3K2X", action: a, entity: "hospital", entityID: "P-ABCD12"})
		// A pseudonym-shaped actor name.
		insertRow(t, row{actor: pseudo.id, hospital: h, action: a})
	}
	// Rows that map, for a list that isn't all bare entries.
	insertRow(t, row{actor: c.id, hospital: h, rid: "0123456789abcdef0123456789abcdef", action: "auth.login.succeeded"})
	insertRow(t, row{actor: c.id, hospital: h, action: "audit.searched", payload: map[string]any{"searched_kind": "request"}})

	scan := func(what, raw string) {
		t.Helper()
		if strings.Contains(raw, marker) || strings.Contains(raw, "SYNTHETIC") {
			t.Errorf("%s carries a marker: %s", what, raw)
		}
		if loc := pseudonymIn.FindStringIndex(raw); loc != nil {
			t.Errorf("%s carries a pseudonym-shaped string %q", what, raw[loc[0]:loc[1]])
		}
	}
	mapped := 0
	cursor := ""
	for page := 0; ; page++ {
		path := "/platform/audit?limit=100&hospital=" + h.String()
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		r := get(t, path, tok)
		ok(t, "list", r)
		// The whole reply, the cursor included (hex: no pseudonym).
		scan("list", r.raw)
		for _, e := range events(t, r) {
			if e["kind"] != "unknown" {
				mapped++
			}
			d := get(t, "/platform/audit/"+e["id"].(string), tok)
			ok(t, "detail", d)
			scan("detail", d.raw)
		}
		next, _ := r.body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
		if page > 50 {
			t.Fatal("paging never ends")
		}
	}
	if mapped < 2 {
		t.Errorf("only %d mapped entries: the scan proves little", mapped)
	}
	hr := get(t, "/platform/audit/hospitals", tok)
	ok(t, "hospitals", hr)
	scan("hospitals", hr.raw)
	pr := get(t, "/platform/audit/people?q=Dr", tok)
	ok(t, "people", pr)
	scan("people", pr.raw)
	for _, x := range pr.body["people"].([]any) {
		if x.(map[string]any)["id"] == pseudo.id.String() {
			t.Errorf("a person with a pseudonym-shaped name is listed")
		}
	}
}

// --- after_break -----------------------------------------------------------------

func TestAuditAfterBreak(t *testing.T) {
	h := seedHospital(t, "active")
	before := insertRow(t, row{hospital: h, action: "auth.logout"})
	broken := insertRow(t, row{hospital: h, action: "auth.logout"})
	after := insertRow(t, row{hospital: h, action: "auth.logout"})
	ctx := context.Background()
	var run uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO audit_verify_runs (state, finished_at, entries_checked, first_broken_seq,
		first_broken_id, first_broken_at) VALUES ('broken', now(), 10, $1, $2, now()) RETURNING id`,
		broken, strconv.FormatInt(broken, 10)).Scan(&run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = super.Exec(ctx, `DELETE FROM audit_verify_runs`) })
	tok := mint(t, auditor(t))
	flags := func() map[int64]any {
		r := get(t, "/platform/audit?hospital="+h.String(), tok)
		ok(t, "list", r)
		out := map[int64]any{}
		for _, e := range events(t, r) {
			out[int64(e["seq"].(float64))] = e["after_break"]
		}
		return out
	}
	f := flags()
	if f[before] != false || f[broken] != false || f[after] != true {
		t.Errorf("open failure: after_break %v (before %d, broken %d, after %d)", f, before, broken, after)
	}
	// A later intact run without the outcome note: still open.
	if _, err := super.Exec(ctx, `INSERT INTO audit_verify_runs (started_at, state, finished_at, entries_checked, from_first, to_seq, to_at)
		VALUES (now() + interval '1 minute', 'intact', now() + interval '2 minutes', 10, true, $1, now())`, after); err != nil {
		t.Fatal(err)
	}
	if f = flags(); f[after] != true {
		t.Errorf("intact without a note cleared the failure: %v", f)
	}
	// With the note too, the failure is closed.
	if _, err := super.Exec(ctx, `UPDATE audit_verify_runs SET outcome_note = 'synthetic note', outcome_by = $2, outcome_at = now()
		WHERE id = $1`, run, auditor(t).id); err != nil {
		t.Fatal(err)
	}
	if f = flags(); f[after] != false {
		t.Errorf("closed failure still marks entries: %v", f)
	}
}

// --- who may read ---------------------------------------------------------------

var auditReads = []string{"/platform/audit", "/platform/audit/1", "/platform/audit/hospitals", "/platform/audit/people?q=ab"}

func TestAuditClinicalAndHospitalRolesRefused(t *testing.T) {
	h := seedHospital(t, "active")
	for _, roles := range [][]string{{authz.RoleClinician}, {authz.RoleHospitalAdmin}, {authz.RoleAnalyst},
		{authz.RoleClinician, authz.RoleHospitalAdmin}} {
		tok := mint(t, seedUser(t, h, "", roles...))
		for _, p := range auditReads {
			problem(t, fmt.Sprint(roles, " platform ", p), do(t, "GET", p, tok, true), 403, "not_platform")
			problem(t, fmt.Sprint(roles, " clinical ", p), do(t, "GET", p, tok, false), 403, "not_platform")
		}
	}
	// A platform account on the clinical audience.
	tok := mint(t, auditor(t))
	for _, p := range auditReads {
		problem(t, "auditor clinical "+p, do(t, "GET", p, tok, false), 403, "not_platform")
	}
	// No token.
	for _, p := range auditReads {
		problem(t, "anonymous "+p, get(t, p, ""), 401, "unauthenticated")
	}
	// Both platform roles read.
	insertRow(t, row{action: "auth.logout"})
	for _, r := range []string{authz.RolePlatformAdmin, authz.RoleAuditor} {
		tok := mint(t, seedUser(t, uuid.Nil, "", r))
		for _, p := range []string{"/platform/audit", "/platform/audit/hospitals", "/platform/audit/people?q=ab"} {
			ok(t, r+" "+p, get(t, p, tok))
		}
	}
}

func TestAuditNeedsTwoStep(t *testing.T) {
	for _, r := range []string{authz.RolePlatformAdmin, authz.RoleAuditor} {
		tok := signIn(t, seedUser(t, uuid.Nil, "", r), false).tok
		for _, p := range auditReads {
			problem(t, r+" without two-step "+p, get(t, p, tok), 403, "mfa_required")
		}
	}
}

func TestDeferredAuditRoutes404(t *testing.T) {
	tok := mint(t, auditor(t))
	id := strconv.FormatInt(insertRow(t, row{action: "auth.logout"}), 10)
	for _, c := range []struct{ method, path string }{
		{"GET", "/platform/audit/" + id + "/patients"},
		{"GET", "/platform/audit/named-access"},
		{"GET", "/platform/audit/named-access?hospital=platform"},
		{"GET", "/platform/audit/exports"},
		{"POST", "/platform/audit/exports"},
		{"GET", "/platform/audit/exports/abc/file"},
		{"POST", "/platform/audit"},
		{"DELETE", "/platform/audit/" + id},
	} {
		problem(t, c.method+" "+c.path, do(t, c.method, c.path, tok, true), 404, "not_found")
	}
	for _, rr := range server.Table() {
		for _, deferred := range []string{"/patients", "/named-access", "/exports"} {
			if strings.HasPrefix(rr.Pattern, httpx.BasePath+"/platform/audit") && strings.Contains(rr.Pattern, deferred) {
				t.Errorf("deferred route registered: %s %s", rr.Method, rr.Pattern)
			}
		}
	}
}

func TestAuditPeopleNeedsTwoChars(t *testing.T) {
	h := seedHospital(t, "active")
	tag := "Pickme" + uniq()
	var made []user
	for i := 0; i < 25; i++ {
		made = append(made, seedUser(t, h, "Dr "+tag+" "+strconv.Itoa(i), authz.RoleClinician))
	}
	plat := seedUser(t, uuid.Nil, "Ms "+tag+" Platform", authz.RolePlatformAdmin)
	tok := mint(t, auditor(t))
	for _, bad := range []string{"", "?q=", "?q=a", "?q=%20a%20", "?q=" + strings.Repeat("a", 101), "?q=ab&q=cd",
		"?q=ab&x=1", "?q=a%00b"} {
		problem(t, "people"+bad, get(t, "/platform/audit/people"+bad, tok), 422, "validation_failed")
	}
	r := get(t, "/platform/audit/people?q="+tag, tok)
	ok(t, "people", r)
	sameKeys(t, "people reply", r.body, "people")
	people := r.body["people"].([]any)
	if len(people) != 20 {
		t.Fatalf("want at most 20 people, got %d", len(people))
	}
	for _, x := range people {
		p := x.(map[string]any)
		sameKeys(t, "person", p, "id", "name", "email", "role", "kind", "hospital")
		if !strings.Contains(p["name"].(string), tag) {
			t.Errorf("person %v doesn't match", p)
		}
		if p["kind"] == "staff" {
			hp := p["hospital"].(map[string]any)
			sameKeys(t, "person's hospital", hp, "id", "name")
			if hp["id"] != h.String() || p["role"] != "clinician" {
				t.Errorf("staff person %v", p)
			}
		}
	}
	// By email, case-insensitively; a platform person has no hospital.
	r = get(t, "/platform/audit/people?q="+url.QueryEscape(strings.ToUpper(plat.email[:14])), tok)
	ok(t, "by email", r)
	found := false
	for _, x := range r.body["people"].([]any) {
		p := x.(map[string]any)
		if p["id"] == plat.id.String() {
			found = true
			if p["hospital"] != nil || p["kind"] != "platform" || p["role"] != "platform_admin" || p["email"] != plat.email {
				t.Errorf("platform person %v", p)
			}
		}
	}
	if !found {
		t.Errorf("email search didn't find %s", plat.email)
	}
	// Wildcards are literal.
	r = get(t, "/platform/audit/people?q=%25%25", tok)
	ok(t, "wildcards", r)
	if n := len(r.body["people"].([]any)); n != 0 {
		t.Errorf("q=%%%% matched %d people", n)
	}
	r = get(t, "/platform/audit/people?q=__", tok)
	ok(t, "underscores", r)
	if n := len(r.body["people"].([]any)); n != 0 {
		t.Errorf("q=__ matched %d people", n)
	}
	_ = made
}

// --- every read path is recorded (Task 9 review) ------------------------

// The detail is recorded as the exact-id search for its id, in the same
// transaction: a walk over ids leaves a row per entry read, and if the row
// can't be written nothing is shown.
func TestAuditDetailIsRecorded(t *testing.T) {
	h := seedHospital(t, "active")
	x := insertRow(t, row{hospital: h, action: "auth.logout"})
	y := insertRow(t, row{hospital: h, action: "auth.logout"})
	a := auditor(t)
	s := signIn(t, a, true)
	xs, ys := strconv.FormatInt(x, 10), strconv.FormatInt(y, 10)
	ok(t, "detail x", get(t, "/platform/audit/"+xs, s.tok))
	searched := actionsBy(t, a, "audit.searched")
	if len(searched) != 1 {
		t.Fatalf("detail: want one audit.searched, got %+v", searched)
	}
	if r := searched[0]; r.entity != "audit_event" || r.entityID != xs || r.hospital != nil ||
		mustJSON(r.payload) != `{"searched_kind":"event"}` {
		t.Errorf("detail row %+v", r)
	}
	// The same id again, or its q search, within 5 minutes: the same read.
	ok(t, "detail x again", get(t, "/platform/audit/"+xs, s.tok))
	ok(t, "q=x", get(t, "/platform/audit?q="+xs, s.tok))
	if n := len(actionsBy(t, a, "audit.searched")); n != 1 {
		t.Errorf("repeats wrote rows: %d", n)
	}
	// Another id, another session: rows of their own.
	ok(t, "detail y", get(t, "/platform/audit/"+ys, s.tok))
	ok(t, "detail x, other session", get(t, "/platform/audit/"+xs, signIn(t, a, true).tok))
	if n := len(actionsBy(t, a, "audit.searched")); n != 3 {
		t.Errorf("want 3 rows, got %d", n)
	}
	// An id with no entry shows nothing and records nothing.
	problem(t, "no such id", get(t, "/platform/audit/987654321098", s.tok), 404, "not_found")
	if n := len(actionsBy(t, a, "audit.searched")); n != 3 {
		t.Errorf("a 404 wrote a row: %d", n)
	}
	// Fail closed: when the row can't be written, 500 and no entry.
	ctx := context.Background()
	var check string
	if err := super.QueryRow(ctx, `SELECT with_check FROM pg_policies WHERE tablename = 'audit_log' AND policyname = 'platform_append'`).
		Scan(&check); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `ALTER POLICY platform_append ON audit_log WITH CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := super.Exec(ctx, `ALTER POLICY platform_append ON audit_log WITH CHECK (`+check+`)`); err != nil {
			t.Errorf("restore platform_append: %v", err)
		}
	})
	z := insertRow(t, row{hospital: h, action: "auth.logout"})
	var hash string
	if err := super.QueryRow(ctx, `SELECT hash FROM audit_log WHERE id = $1`, z).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	r := get(t, "/platform/audit/"+strconv.FormatInt(z, 10), signIn(t, a, true).tok)
	problem(t, "unrecordable detail", r, 500, "internal_error")
	if strings.Contains(r.raw, hash) || strings.Contains(r.raw, "signed_out") {
		t.Errorf("an unrecorded detail leaked the entry: %s", r.raw)
	}
	sameKeys(t, "500 body", r.body, "code")
}

// The person picker and the hospitals list are recorded as audit.searched
// with searched_kind person or hospital, never the text searched; mhai-web
// has no such kind yet, so they are listed as bare entries.
func TestAuditPeopleAndHospitalsRecorded(t *testing.T) {
	a := auditor(t)
	s := signIn(t, a, true)
	tag := "Findme" + uniq()
	ok(t, "people", get(t, "/platform/audit/people?q="+tag, s.tok))
	ok(t, "people again", get(t, "/platform/audit/people?q="+url.QueryEscape("  "+tag+" "), s.tok))
	ok(t, "people other", get(t, "/platform/audit/people?q="+tag+"x", s.tok))
	ok(t, "hospitals", get(t, "/platform/audit/hospitals", s.tok))
	ok(t, "hospitals again", get(t, "/platform/audit/hospitals", s.tok))
	var kinds []string
	for _, r := range actionsBy(t, a, "audit.searched") {
		if r.entity != "" || r.entityID != "" || r.hospital != nil || len(r.payload) != 1 {
			t.Errorf("searched row %+v", r)
		}
		if strings.Contains(mustJSON(r.payload), tag) {
			t.Errorf("the text searched is recorded: %v", r.payload)
		}
		kinds = append(kinds, fmt.Sprint(r.payload["searched_kind"]))
	}
	if want := []string{"person", "person", "hospital"}; !slices.Equal(kinds, want) {
		t.Errorf("searched rows %v, want %v", kinds, want)
	}
	// Listed, but bare until mhai-web takes the kinds.
	r := get(t, "/platform/audit?hospital=platform&person="+a.id.String()+"&type=security", mint(t, auditor(t)))
	ok(t, "list", r)
	es := events(t, r)
	if len(es) != 3 {
		t.Fatalf("want the 3 search rows, got %s", r.raw)
	}
	for _, e := range es {
		sameKeys(t, "person/hospital search entry", e, bareKeys...)
	}
	// An event search still maps.
	ok(t, "event search", get(t, "/platform/audit?q=1", s.tok))
	r = get(t, "/platform/audit?hospital=platform&person="+a.id.String()+"&type=audit", mint(t, auditor(t)))
	ok(t, "list audit", r)
	found := false
	for _, e := range events(t, r) {
		if e["kind"] == "audit_searched" {
			found = true
			if f := e["facts"].(map[string]any); f["searched_kind"] != "event" {
				t.Errorf("event search facts %v", f)
			}
		}
	}
	if !found {
		t.Errorf("the event search isn't listed as audit_searched: %s", r.raw)
	}
}

// People search pages past matches it can't show: 105 analyst-only
// matches sort first, and the 20 clinicians after them are still found.
func TestAuditPeopleSearchFillsPastUnshowable(t *testing.T) {
	h := seedHospital(t, "active")
	tag := "Pagethru" + uniq()
	for i := 0; i < 105; i++ {
		seedUser(t, h, fmt.Sprintf("Dr %s a%03d", tag, i), authz.RoleAnalyst)
	}
	var want []string
	for i := 0; i < 22; i++ {
		u := seedUser(t, h, fmt.Sprintf("Dr %s b%03d", tag, i), authz.RoleClinician)
		want = append(want, u.id.String())
	}
	r := get(t, "/platform/audit/people?q="+tag, mint(t, auditor(t)))
	ok(t, "people", r)
	var got []string
	for _, x := range r.body["people"].([]any) {
		got = append(got, x.(map[string]any)["id"].(string))
	}
	if !slices.Equal(got, want[:20]) {
		t.Errorf("got %d people %v, want the first 20 clinicians", len(got), got)
	}
}
