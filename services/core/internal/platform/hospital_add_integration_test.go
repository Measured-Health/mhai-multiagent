//go:build integration

package platform_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 TOTP uses HMAC-SHA1
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/delivery"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/platform"
)

// POST /platform/hospitals (mhai-web's "Add hospital" form; migration
// 00059) through core's whole chain. Every name, code, domain and email
// here is synthetic.

// testAppURL is the clinical app's address the test server builds links on.
const testAppURL = "https://app.synthetic.example"

// recSender is the test server's sender: it keeps every message handed to
// it after commit (with its link, which only a sender sees), and refuses
// them while failing is set.
type recSender struct {
	mu      sync.Mutex
	msgs    []delivery.Message
	failing bool
}

func (*recSender) Channel() string { return "email" }

func (s *recSender) Send(_ context.Context, m delivery.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		return errors.New("synthetic: not sent")
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *recSender) setFailing(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = v
}

// to are the messages sent to recipient.
func (s *recSender) to(recipient string) []delivery.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []delivery.Message
	for _, m := range s.msgs {
		if m.Recipient.String() == recipient {
			out = append(out, m)
		}
	}
	return out
}

// invites is the test server's sender (TestMain).
var invites = &recSender{}

var firstAdminKeys = []string{"invite_id", "name", "email", "status", "sent_at", "expires_at"}

// addBody is a valid body for short code code and admin email email; set
// changes or removes (nil) top-level fields.
func addBody(t *testing.T, code, email string, set map[string]any) string {
	t.Helper()
	m := map[string]any{
		"name":          "Synthetic Riverside Teaching " + code,
		"short_code":    code,
		"country":       "NG",
		"time_zone":     "Africa/Lagos",
		"email_domains": []string{"Riverside.Synthetic.Example", "riverside.synthetic.example", " staff.synthetic.example "},
		"first_admin":   map[string]any{"name": "Synthetic Admin " + code, "email": email},
	}
	for k, v := range set {
		if v == nil {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return jsonBody(t, m)
}

// coded is body with a fresh backup code of u's added (the step-up).
func coded(t *testing.T, u user, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	m["backup_code"] = backupCode(t, u)
	return jsonBody(t, m)
}

// withTOTP is body with u's current TOTP code: u is enrolled on a fresh
// secret, sealed as enrolment seals it.
func withTOTP(t *testing.T, u user, body string) string {
	t.Helper()
	secret, err := auth.RandomTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := auth.NewTOTPSealer([]byte(testMFAKey))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal(u.id, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(context.Background(), `UPDATE user_mfa SET secret_enc = $2 WHERE user_id = $1`, u.id, sealed); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	m["mfa_code"] = totpNow(secret)
	return jsonBody(t, m)
}

// totpNow is RFC 6238's code for now (SHA-1, 30 s, 6 digits), written
// independently of auth's.
func totpNow(secret []byte) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(time.Now().Unix()/30))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

func newCode() string { return "N" + strings.ToUpper(uniq()[:7]) }

func newEmail() string { return "synthetic.admin." + uniq() + "@riverside.synthetic.example" }

// addKeyed is POST /platform/hospitals with Idempotency-Key key ("" for a
// fresh one) on h.
func addKeyed(t *testing.T, h http.Handler, tok, body, key string) resp {
	t.Helper()
	if key == "" {
		key = "k-" + uniq()
	}
	r := httptest.NewRequest(http.MethodPost, httpx.BasePath+"/platform/hospitals", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("X-MHAI-Audience", "platform")
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	out := resp{code: w.Code, raw: w.Body.String(), hdr: w.Header()}
	_ = json.Unmarshal(w.Body.Bytes(), &out.body)
	return out
}

func addHospital(t *testing.T, tok, body string) resp { return addKeyed(t, handler, tok, body, "") }

func created(t *testing.T, what string, r resp) {
	t.Helper()
	if r.code != http.StatusCreated {
		t.Fatalf("%s: %d %s", what, r.code, r.raw)
	}
}

// count is one counting query's answer.
func count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := super.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// nothingAdded checks no trace of code or email is left.
func nothingAdded(t *testing.T, what, code, email string) {
	t.Helper()
	if n := count(t, `SELECT count(*) FROM hospitals WHERE short_code = $1`, code); n != 0 {
		t.Errorf("%s: %d hospitals with the code", what, n)
	}
	if n := count(t, `SELECT count(*) FROM users WHERE email = $1`, email); n != 0 {
		t.Errorf("%s: %d accounts with the email", what, n)
	}
	if got := invitesTo(t, email); got != 0 {
		t.Errorf("%s: %d messages sent", what, got)
	}
}

// invitesTo counts the messages the sender got for the account email has
// (none when there is none).
func invitesTo(t *testing.T, email string) int {
	t.Helper()
	var id uuid.UUID
	if err := super.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		return 0
	}
	return len(invites.to(id.String()))
}

func hospitalAudits(t *testing.T, hid string) []auditRow {
	t.Helper()
	rows, err := super.Query(context.Background(), `SELECT action, COALESCE(actor_id::text, ''), COALESCE(entity, ''),
		COALESCE(entity_id, ''), hospital_id::text, payload FROM audit_log WHERE hospital_id = $1 ORDER BY id`, hid)
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
	return out
}

func stampTime(t *testing.T, what string, v any) time.Time {
	t.Helper()
	s, _ := v.(string)
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("%s: %v is not a time", what, v)
	}
	return at
}

// A platform admin adds a hospital: 201 with the page GET then reads; the
// hospital set up as the CLI leaves it, its first admin invited (account,
// role, 3-day link, outbox row, one message to the sender) and two audit
// entries by ids.
func TestHospitalAddCreatesAndGetReadsIt(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	code, email := newCode(), newEmail()
	before := time.Now().UTC()
	r := addHospital(t, tok, withTOTP(t, adm, addBody(t, strings.ToLower(code), strings.ToUpper(email), nil)))
	created(t, "add with a TOTP code", r)
	sameKeys(t, "add", r.body, hospitalKeys...)
	hid, _ := r.body["id"].(string)
	want := map[string]any{"short_code": code, "status": "setup_incomplete", "country": "NG", "time_zone": "Africa/Lagos",
		"name": "Synthetic Riverside Teaching " + strings.ToLower(code), "version": float64(1), "sso_enabled": false,
		"deactivation": nil, "removal": nil, "last_activity_at": nil, "legal_holds_active": float64(0)}
	for k, v := range want {
		if !reflect.DeepEqual(r.body[k], v) {
			t.Errorf("%s: %v, want %v", k, r.body[k], v)
		}
	}
	if got := jsonOf(r.body["email_domains"]); got != `["riverside.synthetic.example","staff.synthetic.example"]` {
		t.Errorf("email_domains %s", got)
	}
	if got := jsonOf(r.body["admins"]); got != `[]` {
		t.Errorf("admins %s: an admin who hasn't accepted can't act", got)
	}
	fa, _ := r.body["first_admin"].(map[string]any)
	sameKeys(t, "first_admin", fa, firstAdminKeys...)
	adminID, _ := fa["invite_id"].(string)
	if fa["name"] != "Synthetic Admin "+strings.ToLower(code) || fa["email"] != strings.ToLower(email) || fa["status"] != "pending" {
		t.Errorf("first_admin %v", fa)
	}
	sent, exp := stampTime(t, "sent_at", fa["sent_at"]), stampTime(t, "expires_at", fa["expires_at"])
	if d := exp.Sub(sent); d != platform.FirstAdminInviteTTL {
		t.Errorf("invite lasts %v, want 3 days", d)
	}
	if sent.Before(before.Add(-time.Minute)) {
		t.Errorf("sent_at %v", sent)
	}

	// GET reads exactly what POST answered.
	g := get(t, "/platform/hospitals/"+hid, mint(t, auditor(t)))
	ok(t, "get", g)
	if jsonOf(g.body) != jsonOf(r.body) {
		t.Errorf("GET %s\nPOST %s", g.raw, r.raw)
	}

	// The rows: the hospital as the CLI leaves it (setup_incomplete, not
	// the demo, settings at their defaults), the invited account.
	var status string
	var demo, mfaClin, trust bool
	if err := super.QueryRow(context.Background(), `SELECT h.status, h.is_demo, s.require_mfa_for_clinicians, s.allow_trust_device
		FROM hospitals h JOIN hospital_settings s ON s.hospital_id = h.id WHERE h.id = $1`, hid).Scan(&status, &demo, &mfaClin, &trust); err != nil {
		t.Fatal(err)
	}
	if status != "setup_incomplete" || demo || !mfaClin || trust {
		t.Errorf("hospital %s demo %v settings %v %v", status, demo, mfaClin, trust)
	}
	var invited, hasPassword bool
	var roles []string
	if err := super.QueryRow(context.Background(), `SELECT u.invited_at IS NOT NULL, u.password_hash IS NOT NULL,
		ARRAY(SELECT role FROM user_roles WHERE user_id = u.id) FROM users u WHERE u.id = $1 AND u.hospital_id = $2`,
		adminID, hid).Scan(&invited, &hasPassword, &roles); err != nil {
		t.Fatal(err)
	}
	if !invited || hasPassword || jsonOf(roles) != `["hospital_admin"]` {
		t.Errorf("account invited %v password %v roles %v", invited, hasPassword, roles)
	}
	if n := count(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1 AND purpose = 'invite' AND used_at IS NULL
		AND revoked_at IS NULL AND created_by = $2 AND hospital_id = $3`, adminID, adm.id, hid); n != 1 {
		t.Errorf("%d live invites", n)
	}
	if n := count(t, `SELECT count(*) FROM message_outbox WHERE recipient_id = $1 AND hospital_id = $2 AND kind = 'invite'
		AND channel = 'email' AND created_by = $3`, adminID, hid, adm.id); n != 1 {
		t.Errorf("%d outbox rows", n)
	}

	// One message, its link on the app's reset page; never in the reply.
	msgs := invites.to(adminID)
	if len(msgs) != 1 {
		t.Fatalf("%d messages sent", len(msgs))
	}
	m := msgs[0]
	if m.Kind != delivery.Invite || m.HospitalID.String() != hid || m.By != adm.id ||
		!strings.HasPrefix(m.Link, testAppURL+"/reset-password?token=") || !m.ExpiresAt.Truncate(time.Millisecond).Equal(exp) {
		t.Errorf("message %+v", m)
	}
	if plain := strings.TrimPrefix(m.Link, testAppURL+"/reset-password?token="); strings.Contains(r.raw, plain) {
		t.Error("the reply carries the link's token")
	}

	// Audit: hospital.created via platform and staff.invited, by ids only.
	rows := hospitalAudits(t, hid)
	if len(rows) != 2 {
		t.Fatalf("%d audit rows: %+v", len(rows), rows)
	}
	if a := rows[0]; a.action != "hospital.created" || a.actor != adm.id.String() || a.entity != "hospital" || a.entityID != hid ||
		jsonOf(a.payload) != `{"via":"platform"}` {
		t.Errorf("hospital.created %+v", a)
	}
	if a := rows[1]; a.action != "staff.invited" || a.actor != adm.id.String() || a.entity != "user" || a.entityID != adminID ||
		jsonOf(a.payload) != `{"roles":["hospital_admin"]}` {
		t.Errorf("staff.invited %+v", a)
	}
	for _, a := range rows {
		raw := jsonOf(a.payload)
		if strings.Contains(raw, code) || strings.Contains(strings.ToLower(raw), "synthetic") {
			t.Errorf("audit payload carries input: %s", raw)
		}
	}
}

// The invite works: the link sets the first admin's password, and the
// page then shows them accepted and among the admins. A lapsed invite is
// expired.
func TestHospitalAddInviteAcceptedAndExpired(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	code, email := newCode(), newEmail()
	r := addHospital(t, tok, coded(t, adm, addBody(t, code, email, nil)))
	created(t, "add", r)
	hid := r.body["id"].(string)
	adminID := r.body["first_admin"].(map[string]any)["invite_id"].(string)
	msgs := invites.to(adminID)
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	u, err := url.Parse(msgs[0].Link)
	if err != nil {
		t.Fatal(err)
	}
	body := jsonBody(t, map[string]any{"token": u.Query().Get("token"), "new_password": "Synthetic-Lantern-Orchard-4417!"})
	req := httptest.NewRequest(http.MethodPost, httpx.BasePath+"/auth/password/reset", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MHAI-Service", serviceKey)
	req.Header.Set("X-MHAI-Client-IP", "198.51.100.77")
	req.Header.Set("Idempotency-Key", "k-"+uniq())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sign_in":"clinical"`) {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	g := get(t, "/platform/hospitals/"+hid, tok)
	ok(t, "get", g)
	if s := g.body["first_admin"].(map[string]any)["status"]; s != "accepted" {
		t.Errorf("status %v, want accepted", s)
	}
	if got := jsonOf(g.body["admins"]); got != jsonOf([]map[string]any{{"name": "Synthetic Admin " + code, "email": email}}) {
		t.Errorf("admins %s", got)
	}

	// Another hospital's invite lapses.
	code2, email2 := newCode(), newEmail()
	r = addHospital(t, tok, coded(t, adm, addBody(t, code2, email2, nil)))
	created(t, "add 2", r)
	hid2 := r.body["id"].(string)
	if _, err := super.Exec(context.Background(), `UPDATE account_tokens SET created_at = now() - interval '4 days',
		expires_at = now() - interval '1 day' WHERE hospital_id = $1`, hid2); err != nil {
		t.Fatal(err)
	}
	g = get(t, "/platform/hospitals/"+hid2, tok)
	ok(t, "get 2", g)
	if s := g.body["first_admin"].(map[string]any)["status"]; s != "expired" {
		t.Errorf("status %v, want expired", s)
	}
}

// A hospital the CLI made has no country, domains or first admin.
func TestHospitalPageWithoutFirstAdmin(t *testing.T) {
	h := seedHospital(t, "active")
	g := get(t, "/platform/hospitals/"+h.String(), mint(t, platformAdmin(t)))
	ok(t, "get", g)
	if g.body["country"] != "" || jsonOf(g.body["email_domains"]) != "[]" || g.body["first_admin"] != nil {
		t.Errorf("CLI hospital: %s", g.raw)
	}
}

// A short code another hospital has (any case) is 409 duplicate; an email
// an account has is 409 email_in_use. Nothing is left of either.
func TestHospitalAddConflicts(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	code := newCode()
	created(t, "first", addHospital(t, tok, coded(t, adm, addBody(t, code, newEmail(), nil))))
	// The 409s come before the step-up: no code is given or spent.
	email := newEmail()
	problem(t, "same code", addHospital(t, tok, addBody(t, strings.ToLower(code), email, nil)), 409, "duplicate")
	if n := count(t, `SELECT count(*) FROM users WHERE email = $1`, email); n != 0 {
		t.Errorf("the duplicate left %d accounts", n)
	}

	taken := seedUser(t, seedHospital(t, "active"), "", authz.RoleClinician)
	code2 := newCode()
	problem(t, "email in use", addHospital(t, tok, addBody(t, code2, strings.ToUpper(taken.email), nil)), 409, "email_in_use")
	if n := count(t, `SELECT count(*) FROM hospitals WHERE short_code = $1`, code2); n != 0 {
		t.Errorf("the refused add left %d hospitals", n)
	}
	platformEmail := platformAdmin(t).email
	problem(t, "a platform account's email", addHospital(t, tok, addBody(t, newCode(), platformEmail, nil)), 409, "email_in_use")
}

// Each field is required and checked: 422 validation_failed, nothing added.
func TestHospitalAddBadBody(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	long := strings.Repeat("x", 121)
	manyDomains := []string{}
	for i := 0; i < platform.MaxEmailDomains+1; i++ {
		manyDomains = append(manyDomains, "d"+uniq()+".synthetic.example")
	}
	cases := map[string]map[string]any{
		"no name":            {"name": nil},
		"empty name":         {"name": "  "},
		"long name":          {"name": long},
		"control in name":    {"name": "Synthetic\nGeneral"},
		"name not a string":  {"name": 7},
		"no short code":      {"short_code": nil},
		"code too short":     {"short_code": "A"},
		"short code symbols": {"short_code": "AB C"},
		"no country":         {"country": nil},
		"country name":       {"country": "Nigeria"},
		"country 3 letters":  {"country": "NGA"},
		"country unassigned": {"country": "QQ"},
		"country private":    {"country": "ZZ"},
		"no time zone":       {"time_zone": nil},
		"bad time zone":      {"time_zone": "Mars/Olympus"},
		"local time zone":    {"time_zone": "Local"},
		"no domains":         {"email_domains": nil},
		"domains not a list": {"email_domains": "synthetic.example"},
		"bad domain":         {"email_domains": []string{"not a domain"}},
		"one-label domain":   {"email_domains": []string{"localhost"}},
		"domain with comma":  {"email_domains": []string{"a.synthetic.example,b.synthetic.example"}},
		"null domain":        {"email_domains": []any{nil}},
		"too many domains":   {"email_domains": manyDomains},
		"no first admin":     {"first_admin": nil},
		"admin no name":      {"first_admin": map[string]any{"email": newEmail()}},
		"admin no email":     {"first_admin": map[string]any{"name": "Synthetic Admin"}},
		"admin long name":    {"first_admin": map[string]any{"name": long, "email": newEmail()}},
		"admin bad email":    {"first_admin": map[string]any{"name": "Synthetic Admin", "email": "not-an-email"}},
		"admin named email":  {"first_admin": map[string]any{"name": "Synthetic Admin", "email": "Synthetic <x@synthetic.example>"}},
		"admin extra field":  {"first_admin": map[string]any{"name": "Synthetic Admin", "email": newEmail(), "role": "x"}},
		"unknown field":      {"demo": true},
	}
	for name, set := range cases {
		code, email := newCode(), newEmail()
		problem(t, name, addHospital(t, tok, addBody(t, code, email, set)), 422, "validation_failed")
		nothingAdded(t, name, code, email)
	}
	problem(t, "not JSON", addHospital(t, tok, "{"), 422, "validation_failed")
	r := addKeyed(t, handler, tok, coded(t, adm, addBody(t, newCode(), newEmail(), nil)), "")
	created(t, "a valid body still works", r)
}

// Platform admins only, on a two-step session, on the platform audience.
func TestHospitalAddRoles(t *testing.T) {
	h := seedHospital(t, "active")
	for name, tok := range map[string]string{
		"auditor": mint(t, auditor(t)),
	} {
		code, email := newCode(), newEmail()
		problem(t, name, addHospital(t, tok, addBody(t, code, email, nil)), 403, "forbidden")
		nothingAdded(t, name, code, email)
	}
	for _, roles := range [][]string{{authz.RoleHospitalAdmin}, {authz.RoleClinician}} {
		code, email := newCode(), newEmail()
		problem(t, roles[0], addHospital(t, mint(t, seedUser(t, h, "", roles...)), addBody(t, code, email, nil)), 403, "not_platform")
		nothingAdded(t, roles[0], code, email)
	}
	code, email := newCode(), newEmail()
	problem(t, "no two-step", addHospital(t, signIn(t, platformAdmin(t), false).tok, addBody(t, code, email, nil)), 403, "mfa_required")
	nothingAdded(t, "no two-step", code, email)
	problem(t, "no token", addHospital(t, "", addBody(t, code, email, nil)), 401, "unauthenticated")
	// The clinical audience.
	r := httptest.NewRequest(http.MethodPost, httpx.BasePath+"/platform/hospitals", strings.NewReader(addBody(t, code, email, nil)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-MHAI-Service", serviceKey)
	r.Header.Set("Authorization", "Bearer "+mint(t, platformAdmin(t)))
	r.Header.Set("Idempotency-Key", "k-"+uniq())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("clinical audience: %d %s", w.Code, w.Body.String())
	}
	nothingAdded(t, "clinical audience", code, email)
	// A query string is refused.
	q := send(t, handler, http.MethodPost, "/platform/hospitals?demo=1", mint(t, platformAdmin(t)), addBody(t, code, email, nil))
	problem(t, "a query", q, 422, "validation_failed")
	nothingAdded(t, "a query", code, email)
}

// A retry with the same key gets the same reply and adds nothing: one
// hospital, one account, one invite, one message.
func TestHospitalAddIdempotent(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	code, email := newCode(), newEmail()
	// A backup code works once: the retry is answered from the stored
	// reply, its spent code never checked again.
	body, key := coded(t, adm, addBody(t, code, email, nil)), "k-"+uniq()
	first := addKeyed(t, handler, tok, body, key)
	created(t, "first", first)
	again := addKeyed(t, handler, tok, body, key)
	if again.code != http.StatusCreated || again.raw != first.raw {
		t.Errorf("retry: %d %s\nfirst: %s", again.code, again.raw, first.raw)
	}
	if n := count(t, `SELECT count(*) FROM hospitals WHERE short_code = $1`, code); n != 1 {
		t.Errorf("%d hospitals", n)
	}
	if n := count(t, `SELECT count(*) FROM users WHERE email = $1`, email); n != 1 {
		t.Errorf("%d accounts", n)
	}
	adminID := first.body["first_admin"].(map[string]any)["invite_id"].(string)
	if n := count(t, `SELECT count(*) FROM account_tokens WHERE user_id = $1`, adminID); n != 1 {
		t.Errorf("%d invites", n)
	}
	if n := count(t, `SELECT count(*) FROM message_outbox WHERE recipient_id = $1`, adminID); n != 1 {
		t.Errorf("%d outbox rows", n)
	}
	if n := len(invites.to(adminID)); n != 1 {
		t.Errorf("%d messages sent", n)
	}
	if n := count(t, `SELECT count(*) FROM audit_log WHERE hospital_id = $1`, first.body["id"]); n != 2 {
		t.Errorf("%d audit rows", n)
	}
	// The same key with another body is refused.
	problem(t, "same key, other body", addKeyed(t, handler, tok, addBody(t, newCode(), newEmail(), nil), key), 409, "conflict")
	// Without the key a second add is a duplicate, not a second hospital.
	problem(t, "fresh key", addKeyed(t, handler, tok, body, ""), 409, "duplicate")
}

// With no sender nothing is changed: 503 no_delivery.
func TestHospitalAddNeedsSender(t *testing.T) {
	code, email := newCode(), newEmail()
	problem(t, "no sender", addKeyed(t, emrOff, mint(t, platformAdmin(t)), addBody(t, code, email, nil), ""), 503, "no_delivery")
	nothingAdded(t, "no sender", code, email)
}

// A failure after the last row rolls everything back: no hospital,
// settings, account, role, invite, outbox row, audit entry or message.
func TestHospitalAddRollsBack(t *testing.T) {
	mux := authz.NewMux(guard)
	platform.Register(mux, platform.Deps{Pool: app, Sender: invites, AppURL: testAppURL, Auth: authSvc,
		AfterHospitalAdd: func() error { return errors.New("synthetic failure") }})
	h := mux.Handler([]string{serviceKey})
	before := count(t, `SELECT count(*) FROM audit_log WHERE action IN ('hospital.created', 'staff.invited')`)
	outbox := count(t, `SELECT count(*) FROM message_outbox`)
	tokens := count(t, `SELECT count(*) FROM account_tokens`)
	code, email := newCode(), newEmail()
	adm := platformAdmin(t)
	problem(t, "forced failure", addKeyed(t, h, mint(t, adm), coded(t, adm, addBody(t, code, email, nil)), ""), 500, "internal_error")
	nothingAdded(t, "rolled back", code, email)
	if n := count(t, `SELECT count(*) FROM audit_log WHERE action IN ('hospital.created', 'staff.invited')`); n != before {
		t.Errorf("audit rows %d → %d", before, n)
	}
	if n := count(t, `SELECT count(*) FROM message_outbox`); n != outbox {
		t.Errorf("outbox rows %d → %d", outbox, n)
	}
	if n := count(t, `SELECT count(*) FROM account_tokens`); n != tokens {
		t.Errorf("tokens %d → %d", tokens, n)
	}
	if n := count(t, `SELECT count(*) FROM hospital_settings s JOIN hospitals h ON h.id = s.hospital_id WHERE h.short_code = $1`, code); n != 0 {
		t.Errorf("%d settings rows", n)
	}
}

// A sender that refuses the link: the hospital stays added (its invite
// pending), the reply is 424 not_delivered (not 503 no_delivery, which
// says nothing was changed).
func TestHospitalAddSendFails(t *testing.T) {
	invites.setFailing(true)
	defer invites.setFailing(false)
	adm := platformAdmin(t)
	code, email := newCode(), newEmail()
	problem(t, "not sent", addHospital(t, mint(t, adm), coded(t, adm, addBody(t, code, email, nil))), 424, "not_delivered")
	if n := count(t, `SELECT count(*) FROM hospitals WHERE short_code = $1 AND status = 'setup_incomplete'`, code); n != 1 {
		t.Errorf("%d hospitals kept", n)
	}
}

// A session's adds are counted: past HospitalAddsPerMinute, 429.
func TestHospitalAddRateLimited(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	created(t, "first", addHospital(t, tok, coded(t, adm, addBody(t, newCode(), newEmail(), nil))))
	// Every add that reaches the step-up counts, a refused code included.
	for i := 1; i < platform.HospitalAddsPerMinute; i++ {
		problem(t, "no code", addHospital(t, tok, addBody(t, newCode(), newEmail(), nil)), 403, "mfa_required")
	}
	code2, email := newCode(), newEmail()
	problem(t, "past the limit", addHospital(t, tok, coded(t, adm, addBody(t, code2, email, nil))), 429, "rate_limited")
	nothingAdded(t, "past the limit", code2, email)
	// Another session isn't held up.
	other := platformAdmin(t)
	created(t, "another session", addHospital(t, mint(t, other), coded(t, other, addBody(t, code2, email, nil))))
}

// The step-up: none is 403 mfa_required (not audited), a wrong code or
// both codes 403 mfa_invalid (audited auth.mfa.failed), and nothing is
// added; past MaxMFAFailures wrong codes the account is locked, 429
// mfa_locked (audited auth.mfa.locked), even for a right code.
func TestHospitalAddNeedsStepUp(t *testing.T) {
	adm := platformAdmin(t)
	tok := mint(t, adm)
	code, email := newCode(), newEmail()
	body := addBody(t, code, email, nil)
	problem(t, "no code", addHospital(t, tok, body), 403, "mfa_required")
	if n := mfaRows(t, adm, "auth.mfa.failed", "method", "step_up"); n != 0 {
		t.Errorf("no code audited %d rows", n)
	}
	problem(t, "wrong code", addHospital(t, tok, addBody(t, code, email, map[string]any{"backup_code": "wrong" + uniq()})), 403, "mfa_invalid")
	if n := mfaRows(t, adm, "auth.mfa.failed", "method", "step_up"); n != 1 {
		t.Errorf("auth.mfa.failed rows after a wrong code: %d", n)
	}
	both := addBody(t, code, email, map[string]any{"backup_code": backupCode(t, adm), "mfa_code": "123456"})
	problem(t, "both codes", addHospital(t, tok, both), 403, "mfa_invalid")
	nothingAdded(t, "refused codes", code, email)
	created(t, "a backup code", addHospital(t, tok, coded(t, adm, body)))

	// Lockout, on its own account (the counter is the account's).
	locked := platformAdmin(t)
	ltok := mint(t, locked)
	code, email = newCode(), newEmail()
	got := 0
	for i := 0; i <= auth.MaxMFAFailures && got != http.StatusTooManyRequests; i++ {
		r := addHospital(t, ltok, addBody(t, code, email, map[string]any{"backup_code": "wrong" + uniq()}))
		got = r.code
		if got != http.StatusTooManyRequests && (got != 403 || r.body["code"] != "mfa_invalid") {
			t.Fatalf("wrong code %d: %d %s", i, r.code, r.raw)
		}
	}
	if got != http.StatusTooManyRequests {
		t.Fatalf("not locked after %d wrong codes", auth.MaxMFAFailures+1)
	}
	problem(t, "a right code while locked", addHospital(t, ltok, coded(t, locked, addBody(t, code, email, nil))), 429, "mfa_locked")
	if n := mfaRows(t, locked, "auth.mfa.locked", "reason", "account_attempts"); n < 1 {
		t.Errorf("auth.mfa.locked rows: %d", n)
	}
	nothingAdded(t, "locked", code, email)
}

// mhai_platform reaches only the hospital its own transaction adds: it
// can't put an account, a role, an invite, an outbox row or a
// hospital.created entry on an existing hospital, nor add an active or
// demo hospital, nor set a password.
func TestHospitalAddGrantsHoldToTheNewHospital(t *testing.T) {
	ctx := context.Background()
	existing := seedHospital(t, "setup_incomplete")
	adm := platformAdmin(t)
	try := func(what, q string, args ...any) {
		t.Helper()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE mhai_platform`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, q, args...); err == nil {
			if err := tx.Commit(ctx); err == nil {
				t.Errorf("%s: allowed", what)
			}
		}
	}
	uid := uuid.New()
	try("account on an existing hospital", `INSERT INTO users (id, hospital_id, email, display_name, invited_at)
		VALUES ($1, $2, $3, 'Synthetic', now())`, uid, existing, newEmail())
	try("password", `INSERT INTO users (id, hospital_id, email, display_name, password_hash) VALUES ($1, $2, $3, 'S', 'x')`,
		uid, existing, newEmail())
	try("platform account", `INSERT INTO users (id, email, display_name, invited_at) VALUES ($1, $2, 'S', now())`, uid, newEmail())
	someone := seedUser(t, existing, "", authz.RoleClinician)
	try("role on an existing hospital", `INSERT INTO user_roles (user_id, role) VALUES ($1, 'hospital_admin')`, someone.id)
	try("invite on an existing hospital", `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES ($1, $2, 'invite', $3, $4, now() + interval '1 day')`, existing, someone.id, strings.Repeat("a", 64), adm.id)
	try("reset token", `INSERT INTO account_tokens (hospital_id, user_id, purpose, token_hash, created_by, expires_at)
		VALUES ($1, $2, 'password_reset', $3, $4, now() + interval '1 day')`, existing, someone.id, strings.Repeat("b", 64), adm.id)
	try("outbox on an existing hospital", `INSERT INTO message_outbox (hospital_id, recipient_id, kind, channel, created_by)
		VALUES ($1, $2, 'invite', 'email', $3)`, existing, someone.id, adm.id)
	try("settings on an existing hospital", `INSERT INTO hospital_settings (hospital_id) VALUES ($1)`, existing)
	try("active hospital", `INSERT INTO hospitals (id, name, short_code, country, first_admin_id, status) VALUES ($1, 'S', $2, 'NG', $3, 'active')`,
		uuid.New(), newCode(), uid)
	try("demo hospital", `INSERT INTO hospitals (id, name, short_code, country, first_admin_id, is_demo) VALUES ($1, 'S', $2, 'NG', $3, true)`,
		uuid.New(), newCode(), uid)
	try("hospital without a first admin", `INSERT INTO hospitals (id, name, short_code, country) VALUES ($1, 'S', $2, 'NG')`,
		uuid.New(), newCode())
	try("a first admin who never comes", `INSERT INTO hospitals (id, name, short_code, country, first_admin_id) VALUES ($1, 'S', $2, 'NG', $3)`,
		uuid.New(), newCode(), uuid.New())
	try("audit on an existing hospital", `INSERT INTO audit_log (ts, actor_id, hospital_id, action, entity, entity_id, payload, prev_hash, hash)
		VALUES (now(), $1, $2, 'hospital.created', 'hospital', $2::text, '{"via":"platform"}', $3, $4)`, adm.id, existing, randHex(), randHex())
	try("token hashes", `SELECT token_hash FROM account_tokens LIMIT 1`)
}
