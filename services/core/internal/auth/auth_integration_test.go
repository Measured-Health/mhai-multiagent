//go:build integration

package auth_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/auth"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/logx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/password"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

const serviceKey = "test-only-bff-service-key-0123456789abcdef"

// testPassword is synthetic; it is inserted as a hash directly, not
// through the policy.
const testPassword = "violet-harbour-lantern-0731"

var (
	super, app *pgxpool.Pool
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
		nc, js, err := bus.Connect(ctx, n.GatewayURL, "", bus.GatewayInboxPrefix)
		if err != nil {
			fmt.Fprintln(os.Stderr, "nats connect:", err)
			return 1
		}
		defer nc.Close()
		if buckets, err = bus.OpenBuckets(ctx, js); err != nil {
			fmt.Fprintln(os.Stderr, "buckets:", err)
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

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	svc   *auth.Service
	h     http.Handler
	clock *clock
	logs  *syncBuffer
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

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{clock: &clock{t: time.Now()}, logs: &syncBuffer{}}
	e.svc = &auth.Service{
		Pool:    app,
		Buckets: buckets,
		Issuer:  issuer,
		Keys:    issuer.PublicKeys(),
		Log:     logx.New(e.logs),
		Now:     e.clock.Now,
	}
	rt := httpx.NewRouter()
	e.svc.Register(rt)
	// A probe route behind the token check, standing in for later tasks.
	rt.Handle(http.MethodGet, "/probe", e.svc.RequireToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := httpx.PrincipalFrom(r.Context())
		if !ok {
			httpx.Error(w, r, "internal_error")
			return
		}
		httpx.JSON(w, 200, map[string]string{"user_id": p.UserID})
	})))
	e.h = rt.Handler([]string{serviceKey})
	return e
}

type req struct {
	method, path string
	body         any
	ip           string
	platform     bool
	bearer       string
}

type resp struct {
	code int
	hdr  http.Header
	body map[string]any
	raw  string
}

func (e *env) do(t *testing.T, r req) resp {
	t.Helper()
	var rd *bytes.Reader
	switch b := r.body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	hr := httptest.NewRequest(r.method, httpx.BasePath+r.path, rd)
	hr.Header.Set("X-MHAI-Service", serviceKey)
	hr.Header.Set("Content-Type", "application/json")
	ip := r.ip
	if ip == "" {
		ip = "198.51.100.7"
	}
	hr.Header.Set("X-MHAI-Client-IP", ip)
	if r.platform {
		hr.Header.Set("X-MHAI-Audience", "platform")
	}
	if r.bearer != "" {
		hr.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, hr)
	out := resp{code: rec.Code, hdr: rec.Header(), raw: rec.Body.String()}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out.body)
	}
	return out
}

func (e *env) login(t *testing.T, email, pw, ip string) resp {
	t.Helper()
	return e.do(t, req{method: "POST", path: "/auth/login", body: map[string]any{"username": email, "password": pw}, ip: ip})
}

func uniq() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

type hospitalOpts struct {
	requireMFAClinicians, allowTrust bool
	status                           string
}

func seedHospital(t *testing.T, o hospitalOpts) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if o.status == "" {
		o.status = "active"
	}
	if err := super.QueryRow(ctx, `INSERT INTO hospitals (name, short_code, status) VALUES ('Synthetic General', $1, $2) RETURNING id`,
		"H"+uniq(), o.status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO hospital_settings (hospital_id, idle_timeout_minutes, review_overdue_hours,
		require_mfa_for_clinicians, allow_trust_device) VALUES ($1, 20, 6, $2, $3)`, id, o.requireMFAClinicians, o.allowTrust); err != nil {
		t.Fatal(err)
	}
	return id
}

type user struct {
	id    uuid.UUID
	email string
}

// seedUser creates an account with testPassword. hospital is uuid.Nil for
// platform accounts.
func seedUser(t *testing.T, hospital uuid.UUID, mfa bool, roles ...string) user {
	t.Helper()
	ctx := context.Background()
	email := "synthetic.user." + uniq() + "@havana.example"
	var hid any
	if hospital != uuid.Nil {
		hid = hospital
	}
	var id uuid.UUID
	if err := super.QueryRow(ctx, `INSERT INTO users (hospital_id, email, password_hash, display_name, full_name, role_title)
		VALUES ($1, $2, $3, 'Dr Synthetic', 'Synthetic Person', 'Registrar') RETURNING id`, hid, email, pwHash).Scan(&id); err != nil {
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
	return user{id: id, email: email}
}

func clinician(t *testing.T) user {
	return seedUser(t, seedHospital(t, hospitalOpts{}), false, "clinician")
}

func newIP() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("203.0.%d.%d", b[0], b[1]%250+1)
}

// --- Sign-in ---------------------------------------------------------------

var userKeys = []string{"id", "role", "roles", "email", "display_name", "full_name", "role_title", "specialty",
	"hospital", "mfa_enabled", "must_change_password", "terms_accepted_at", "recovery_codes_left",
	"idle_timeout_minutes", "review_overdue_hours"}

func TestLoginSuccessShape(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	// A clinician-only account takes the token branch (analyst needs MFA).
	u2 := seedUser(t, hid, false, "clinician")

	r := e.login(t, "  "+strings.ToUpper(u2.email)+" ", testPassword, newIP())
	if r.code != 200 {
		t.Fatalf("status %d: %s", r.code, r.raw)
	}
	if cc := r.hdr.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	topKeys := keys(r.body)
	if strings.Join(topKeys, ",") != "access_token,expires_in,refresh_token,user" {
		t.Fatalf("top-level keys %v", topKeys)
	}
	if r.body["expires_in"].(float64) != 900 {
		t.Fatalf("expires_in %v", r.body["expires_in"])
	}
	usr := r.body["user"].(map[string]any)
	got := keys(usr)
	want := append([]string(nil), userKeys...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("user keys\n got %v\nwant %v", got, want)
	}
	if usr["id"] != u2.id.String() || usr["email"] != u2.email || usr["role"] != "clinician" {
		t.Fatalf("user %v", usr)
	}
	if h := usr["hospital"].(map[string]any); h["id"] != hid.String() || h["name"] != "Synthetic General" || len(h) != 2 {
		t.Fatalf("hospital %v", h)
	}
	if usr["idle_timeout_minutes"].(float64) != 20 || usr["review_overdue_hours"].(float64) != 6 {
		t.Fatalf("settings %v %v", usr["idle_timeout_minutes"], usr["review_overdue_hours"])
	}
	if usr["mfa_enabled"] != false || usr["recovery_codes_left"].(float64) != 0 || usr["terms_accepted_at"] != nil {
		t.Fatalf("user flags %v", usr)
	}

	c, err := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	if err != nil {
		t.Fatalf("access token: %v", err)
	}
	if c.Sub != u2.id.String() || c.Hid != hid.String() || c.Sid == "" || c.Jti == "" || c.MFA {
		t.Fatalf("claims %+v", c)
	}
	_, p := jwtPayload(t, r.body["access_token"].(string))
	if p["aud"] != "mhai-bff" || p["exp"].(float64)-p["iat"].(float64) != 900 {
		t.Fatalf("aud %v exp-iat %v", p["aud"], p["exp"].(float64)-p["iat"].(float64))
	}

	// Refresh tokens are stored only as hashes.
	rt := r.body["refresh_token"].(string)
	var n int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, rt).Scan(&n)
	if n != 0 {
		t.Fatal("refresh token stored in plain")
	}
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, token.HashRefresh(rt)).Scan(&n)
	if n != 1 {
		t.Fatal("refresh token hash not stored")
	}

	// The access token works on a protected route.
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: r.body["access_token"].(string)}); pr.code != 200 || pr.body["user_id"] != u2.id.String() {
		t.Fatalf("probe %d %s", pr.code, pr.raw)
	}
	assertAudit(t, r, "auth.login.succeeded", u2.email)
}

func TestLoginWrongPasswordGeneric(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	ip := newIP()
	wrong := e.login(t, u.email, "not-the-password-at-all", ip)
	unknown := e.login(t, "nobody."+uniq()+"@havana.example", "not-the-password-at-all", newIP())
	if wrong.code != 401 || wrong.body["code"] != "unauthenticated" {
		t.Fatalf("wrong password: %d %s", wrong.code, wrong.raw)
	}
	sameReply(t, wrong, unknown)
	assertAudit(t, wrong, "auth.login.failed", u.email)
	assertAudit(t, unknown, "auth.login.failed", "")
}

func TestLoginUnknownSameReply(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test skipped under -short (Ruling 5)")
	}
	e := newEnv(t)
	u := clinician(t)
	var wrong, unknown []time.Duration
	for i := 0; i < 7; i++ {
		start := time.Now()
		a := e.login(t, u.email, "not-the-password-at-all", newIP())
		wrong = append(wrong, time.Since(start))
		start = time.Now()
		b := e.login(t, "nobody."+uniq()+"@havana.example", "not-the-password-at-all", newIP())
		unknown = append(unknown, time.Since(start))
		sameReply(t, a, b)
	}
	mw, mu := median(wrong), median(unknown)
	ratio := float64(mu) / float64(mw)
	t.Logf("median wrong-password %v, unknown %v (ratio %.2f)", mw, mu, ratio)
	if ratio < 0.7 || ratio > 1.3 {
		t.Fatalf("unknown-login median %v not within 30%% of wrong-password median %v", mu, mw)
	}
}

func TestLoginInactiveSameReply(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	_, _ = super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u.id)
	a := e.login(t, u.email, testPassword, newIP())
	b := e.login(t, "nobody."+uniq()+"@havana.example", testPassword, newIP())
	sameReply(t, a, b)

	gone := seedUser(t, seedHospital(t, hospitalOpts{status: "deactivated"}), false, "clinician")
	c := e.login(t, gone.email, testPassword, newIP())
	sameReply(t, c, b)
}

func TestLoginValidation(t *testing.T) {
	e := newEnv(t)
	for _, body := range []any{
		`{"username":"a@havana.example"}`,
		`{"username":"","password":"x"}`,
		`{"username":"a@havana.example","password":"x","extra":1}`,
		`not json`,
	} {
		if r := e.do(t, req{method: "POST", path: "/auth/login", body: body}); r.code != 422 {
			t.Errorf("%v: status %d", body, r.code)
		}
	}
}

func TestLockoutAfterFive(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	ip := newIP()
	for i := 0; i < 5; i++ {
		if r := e.login(t, u.email, "not-the-password-at-all", ip); r.code != 401 {
			t.Fatalf("attempt %d: %d", i+1, r.code)
		}
	}
	r := e.login(t, u.email, testPassword, ip)
	if r.code != 429 || r.body["code"] != "login_locked" {
		t.Fatalf("6th attempt: %d %s", r.code, r.raw)
	}
	assertAudit(t, r, "auth.login.locked", u.email)
	// The login is case- and space-insensitive for the counter too.
	if r := e.login(t, " "+strings.ToUpper(u.email), testPassword, ip); r.code != 429 {
		t.Fatalf("case variant: %d", r.code)
	}
	// Another client IP is unaffected.
	if r := e.login(t, u.email, testPassword, newIP()); r.code != 200 {
		t.Fatalf("other ip: %d %s", r.code, r.raw)
	}
}

func TestLockoutUnknownLoginSame(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	ghost := "nobody." + uniq() + "@havana.example"
	ipA, ipB := newIP(), newIP()
	for i := 0; i < 5; i++ {
		e.login(t, u.email, "not-the-password-at-all", ipA)
		e.login(t, ghost, "not-the-password-at-all", ipB)
	}
	a := e.login(t, u.email, testPassword, ipA)
	b := e.login(t, ghost, testPassword, ipB)
	if a.code != 429 {
		t.Fatalf("known locked: %d", a.code)
	}
	sameReply(t, a, b)
}

func TestLockedRequestsDoNotExtendCounter(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	ip := newIP()
	for i := 0; i < 5; i++ {
		e.login(t, u.email, "not-the-password-at-all", ip)
	}
	k := auth.LockoutKey(u.email, ip)
	before, err := buckets.LoginAttempts.Get(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		e.login(t, u.email, "not-the-password-at-all", ip)
	}
	after, err := buckets.LoginAttempts.Get(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision() != before.Revision() {
		t.Fatal("a locked request wrote the counter")
	}
	if strings.Contains(k, "@") || len(k) != 64 {
		t.Fatalf("lockout key is not a sha256 hex digest")
	}
}

func TestMFARequiredBranch(t *testing.T) {
	e := newEnv(t)
	u := seedUser(t, seedHospital(t, hospitalOpts{}), true, "hospital_admin")
	r := e.login(t, u.email, testPassword, newIP())
	if r.code != 200 {
		t.Fatalf("status %d %s", r.code, r.raw)
	}
	if strings.Join(keys(r.body), ",") != "mfa_required,mfa_token" || r.body["mfa_required"] != true {
		t.Fatalf("body keys %v", keys(r.body))
	}
	tok, _ := r.body["mfa_token"].(string)
	if len(tok) < 32 {
		t.Fatal("mfa_token too short")
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatal("no Cache-Control: no-store")
	}
	// Stored only by hash, with 5 minutes and 5 attempts.
	rec, err := e.svc.MFATokens().Lookup(context.Background(), tok)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if rec.UserID != u.id.String() || rec.AttemptsLeft != 5 || rec.ExpiresAt.Sub(e.clock.Now()) != 5*time.Minute {
		t.Fatalf("record %+v", rec)
	}
	if _, err := buckets.MFAAttempts.Get(context.Background(), tok); err == nil {
		t.Fatal("mfa_token stored in plain as a key")
	}
	// No session was opened.
	var n int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id = $1`, u.id).Scan(&n)
	if n != 0 {
		t.Fatalf("%d sessions opened before MFA", n)
	}
	// Clinicians need MFA when the hospital says so.
	c := seedUser(t, seedHospital(t, hospitalOpts{requireMFAClinicians: true}), true, "clinician")
	if r := e.login(t, c.email, testPassword, newIP()); r.body["mfa_required"] != true {
		t.Fatalf("clinician with hospital policy: %s", r.raw)
	}
}

func TestMFATokenSingleUseAndExpiry(t *testing.T) {
	e := newEnv(t)
	u := seedUser(t, seedHospital(t, hospitalOpts{}), true, "analyst")
	r := e.login(t, u.email, testPassword, newIP())
	tok := r.body["mfa_token"].(string)
	store := e.svc.MFATokens()
	ctx := context.Background()
	if _, err := store.Consume(ctx, tok); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := store.Consume(ctx, tok); err == nil {
		t.Fatal("consumed twice")
	}
	r = e.login(t, u.email, testPassword, newIP())
	tok = r.body["mfa_token"].(string)
	for i := 0; i < 5; i++ {
		rec, err := store.Attempt(ctx, tok)
		if err != nil || rec.AttemptsLeft != 4-i {
			t.Fatalf("attempt %d: left %d, %v", i+1, rec.AttemptsLeft, err)
		}
	}
	if _, err := store.Attempt(ctx, tok); err == nil {
		t.Fatal("a 6th attempt was allowed")
	}
	if _, err := store.Lookup(ctx, tok); err == nil {
		t.Fatal("usable after 5 attempts")
	}
	r = e.login(t, u.email, testPassword, newIP())
	tok = r.body["mfa_token"].(string)
	e.clock.Add(5*time.Minute + time.Second)
	if _, err := store.Lookup(ctx, tok); err == nil {
		t.Fatal("usable after 5 minutes")
	}
}

func TestPlatformAudienceRefusesClinical(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	r := e.do(t, req{method: "POST", path: "/auth/login", platform: true,
		body: map[string]any{"username": u.email, "password": testPassword}, ip: newIP()})
	if r.code != 403 || r.raw != `{"code":"not_platform"}` {
		t.Fatalf("status %d %s", r.code, r.raw)
	}
	// A wrong password on the platform audience is still the generic reply:
	// not_platform is only revealed after the password is verified.
	r = e.do(t, req{method: "POST", path: "/auth/login", platform: true,
		body: map[string]any{"username": u.email, "password": "not-the-password-at-all"}, ip: newIP()})
	if r.code != 401 {
		t.Fatalf("wrong password on platform: %d %s", r.code, r.raw)
	}
	// An admin with every clinical role is still refused.
	a := seedUser(t, seedHospital(t, hospitalOpts{}), true, "clinician", "hospital_admin", "analyst")
	r = e.do(t, req{method: "POST", path: "/auth/login", platform: true,
		body: map[string]any{"username": a.email, "password": testPassword}, ip: newIP()})
	if r.code != 403 || r.raw != `{"code":"not_platform"}` {
		t.Fatalf("clinical admin: %d %s", r.code, r.raw)
	}
	// And a platform account gets the MFA branch.
	p := seedUser(t, uuid.Nil, true, "platform_admin")
	r = e.do(t, req{method: "POST", path: "/auth/login", platform: true,
		body: map[string]any{"username": p.email, "password": testPassword}, ip: newIP()})
	if r.code != 200 || r.body["mfa_required"] != true {
		t.Fatalf("platform admin: %d %s", r.code, r.raw)
	}
}

// --- Refresh ---------------------------------------------------------------

func loggedIn(t *testing.T, e *env) (user, resp) {
	t.Helper()
	u := clinician(t)
	r := e.login(t, u.email, testPassword, newIP())
	if r.code != 200 {
		t.Fatalf("login %d %s", r.code, r.raw)
	}
	return u, r
}

func (e *env) refresh(t *testing.T, rt string) resp {
	t.Helper()
	return e.do(t, req{method: "POST", path: "/auth/refresh", body: map[string]any{"refresh_token": rt}})
}

func TestRefreshRotates(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	old := l.body["refresh_token"].(string)
	r := e.refresh(t, old)
	if r.code != 200 {
		t.Fatalf("refresh %d %s", r.code, r.raw)
	}
	if strings.Join(keys(r.body), ",") != "access_token,expires_in,refresh_token" || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("shape %v", keys(r.body))
	}
	if r.body["refresh_token"] == old {
		t.Fatal("refresh token not rotated")
	}
	c, err := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	c0, _ := token.Verify(l.body["access_token"].(string), issuer.PublicKeys())
	if err != nil || c.Sub != u.id.String() || c.Sid != c0.Sid || c.Jti == c0.Jti {
		t.Fatalf("claims %+v (%v)", c, err)
	}
	// Past the grace window, the old token is unusable (and revokes the family).
	e.clock.Add(11 * time.Second)
	if r2 := e.refresh(t, old); r2.code != 401 {
		t.Fatalf("old token reuse: %d %s", r2.code, r2.raw)
	}
	if r3 := e.refresh(t, "not-a-real-refresh-token"); r3.code != 401 {
		t.Fatalf("garbage: %d", r3.code)
	}
}

func TestRefreshRaceWithin10s(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	old := l.body["refresh_token"].(string)

	// Two tabs refresh the same token at once: one wins, one gets 409.
	var wg sync.WaitGroup
	results := make([]resp, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = e.refresh(t, old) }(i)
	}
	wg.Wait()
	codes := []int{results[0].code, results[1].code}
	sort.Ints(codes)
	if codes[0] != 200 || codes[1] != 409 {
		t.Fatalf("codes %v: %s / %s", codes, results[0].raw, results[1].raw)
	}
	winner := results[0]
	loser := results[1]
	if winner.code != 200 {
		winner, loser = loser, winner
	}
	if loser.body["code"] != "conflict" {
		t.Fatalf("loser %s", loser.raw)
	}
	// Again within 10 s: still 409, nothing revoked.
	e.clock.Add(9 * time.Second)
	if r := e.refresh(t, old); r.code != 409 {
		t.Fatalf("reuse at 9s: %d", r.code)
	}
	// The family stays valid: the newer token refreshes, and the access token works.
	r := e.refresh(t, winner.body["refresh_token"].(string))
	if r.code != 200 {
		t.Fatalf("newer token: %d %s", r.code, r.raw)
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: r.body["access_token"].(string)}); pr.code != 200 {
		t.Fatalf("probe %d", pr.code)
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: l.body["access_token"].(string)}); pr.code != 200 {
		t.Fatalf("original access token revoked by a race: %d", pr.code)
	}
}

func TestRefreshReuseAfter10sRevokesFamily(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	old := l.body["refresh_token"].(string)
	r := e.refresh(t, old)
	if r.code != 200 {
		t.Fatalf("refresh %d", r.code)
	}
	live, _ := token.Verify(r.body["access_token"].(string), issuer.PublicKeys())
	e.clock.Add(10*time.Second + time.Millisecond)
	reuse := e.refresh(t, old)
	if reuse.code != 401 || reuse.body["code"] != "unauthenticated" {
		t.Fatalf("reuse: %d %s", reuse.code, reuse.raw)
	}
	ctx := context.Background()
	var revoked bool
	_ = super.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM sessions WHERE id = $1`, live.Sid).Scan(&revoked)
	if !revoked {
		t.Fatal("session not revoked")
	}
	if !e.svc.Denylisted(ctx, live) {
		t.Fatal("live access jti not denylisted")
	}
	if _, err := buckets.Denylist.Get(ctx, "jti."+live.Jti); err != nil {
		t.Fatalf("jti key: %v", err)
	}
	// The newer refresh token is dead too, and the access token is refused.
	if r2 := e.refresh(t, r.body["refresh_token"].(string)); r2.code != 401 {
		t.Fatalf("newer refresh: %d", r2.code)
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: r.body["access_token"].(string)}); pr.code != 401 {
		t.Fatalf("live access: %d", pr.code)
	}
	var n int
	_ = super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'auth.refresh.reuse' AND actor_id = $1`, u.id).Scan(&n)
	if n != 1 {
		t.Fatalf("reuse audit rows %d", n)
	}
}

func TestRefreshIdleTimeout(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	e.clock.Add(21 * time.Minute) // the hospital's idle timeout is 20
	if r := e.refresh(t, l.body["refresh_token"].(string)); r.code != 401 {
		t.Fatalf("idle refresh: %d", r.code)
	}
}

func TestRefreshInactiveUser(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	_, _ = super.Exec(context.Background(), `UPDATE users SET is_active = false WHERE id = $1`, u.id)
	if r := e.refresh(t, l.body["refresh_token"].(string)); r.code != 401 {
		t.Fatalf("inactive refresh: %d", r.code)
	}
}

// --- Token check, logout, sessions, terms -------------------------------------

func TestRequireToken(t *testing.T) {
	e := newEnv(t)
	_, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	forged, _, _ := token.Issuer{Key: other, KeyID: "test-kid"}.Access(token.Claims{Sub: "x", Sid: "y", Jti: "z"})
	for name, h := range map[string]string{"none": "", "forged": "Bearer " + forged, "basic": "Basic " + at, "bare": at} {
		hr := httptest.NewRequest("GET", httpx.BasePath+"/probe", nil)
		hr.Header.Set("X-MHAI-Service", serviceKey)
		if h != "" {
			hr.Header.Set("Authorization", h)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, hr)
		if rec.Code != 401 {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestLogoutDenylists(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	// A refresh first, so an older access token is also live.
	r := e.refresh(t, l.body["refresh_token"].(string))
	at2 := r.body["access_token"].(string)
	if out := e.do(t, req{method: "POST", path: "/auth/logout", bearer: at2}); out.code != 204 {
		t.Fatalf("logout %d %s", out.code, out.raw)
	}
	for _, tok := range []string{at, at2} {
		if pr := e.do(t, req{method: "GET", path: "/probe", bearer: tok}); pr.code != 401 {
			t.Fatalf("token still accepted after logout: %d", pr.code)
		}
	}
	if rr := e.refresh(t, r.body["refresh_token"].(string)); rr.code != 401 {
		t.Fatalf("refresh after logout: %d", rr.code)
	}
	var n int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'auth.logout' AND actor_id = $1`, u.id).Scan(&n)
	if n != 1 {
		t.Fatalf("logout audit rows %d", n)
	}
}

func TestSessionsList(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	a := e.login(t, u.email, testPassword, "203.0.113.45")
	e.login(t, u.email, testPassword, newIP())
	r := e.do(t, req{method: "GET", path: "/auth/sessions", bearer: a.body["access_token"].(string)})
	if r.code != 200 {
		t.Fatalf("list %d %s", r.code, r.raw)
	}
	list := r.body["sessions"].([]any)
	if len(list) != 2 {
		t.Fatalf("%d sessions", len(list))
	}
	currents := 0
	for _, s := range list {
		m := s.(map[string]any)
		if m["current"] == true {
			currents++
			if m["ip_prefix"] != "203.0.113.0/24" {
				t.Fatalf("ip_prefix %v", m["ip_prefix"])
			}
		}
		for _, k := range []string{"id", "created_at", "last_seen_at", "current"} {
			if _, ok := m[k]; !ok {
				t.Fatalf("session missing %s", k)
			}
		}
	}
	if currents != 1 {
		t.Fatalf("%d current sessions", currents)
	}
}

func TestRevokeAllKeepsNone(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	a := e.login(t, u.email, testPassword, newIP())
	b := e.login(t, u.email, testPassword, newIP())
	if r := e.do(t, req{method: "POST", path: "/auth/sessions/revoke-all", bearer: a.body["access_token"].(string)}); r.code != 204 {
		t.Fatalf("revoke-all %d %s", r.code, r.raw)
	}
	var live int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, u.id).Scan(&live)
	if live != 0 {
		t.Fatalf("%d sessions still live", live)
	}
	for _, x := range []resp{a, b} {
		if pr := e.do(t, req{method: "GET", path: "/probe", bearer: x.body["access_token"].(string)}); pr.code != 401 {
			t.Fatalf("access token survived revoke-all: %d", pr.code)
		}
		if rr := e.refresh(t, x.body["refresh_token"].(string)); rr.code != 401 {
			t.Fatalf("refresh survived revoke-all: %d", rr.code)
		}
	}
}

func TestDeleteOwnSession(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	a := e.login(t, u.email, testPassword, newIP())
	b := e.login(t, u.email, testPassword, newIP())
	cb, _ := token.Verify(b.body["access_token"].(string), issuer.PublicKeys())
	if r := e.do(t, req{method: "DELETE", path: "/auth/sessions/" + cb.Sid, bearer: a.body["access_token"].(string)}); r.code != 204 {
		t.Fatalf("delete %d %s", r.code, r.raw)
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: b.body["access_token"].(string)}); pr.code != 401 {
		t.Fatalf("deleted session's token: %d", pr.code)
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: a.body["access_token"].(string)}); pr.code != 200 {
		t.Fatalf("own token: %d", pr.code)
	}
}

func TestDeleteOtherUsersSession404(t *testing.T) {
	e := newEnv(t)
	_, a := loggedIn(t, e)
	_, b := loggedIn(t, e)
	cb, _ := token.Verify(b.body["access_token"].(string), issuer.PublicKeys())
	for _, id := range []string{cb.Sid, uuid.NewString(), "not-a-uuid"} {
		r := e.do(t, req{method: "DELETE", path: "/auth/sessions/" + id, bearer: a.body["access_token"].(string)})
		if r.code != 404 || r.body["code"] != "not_found" {
			t.Fatalf("%s: %d %s", id, r.code, r.raw)
		}
	}
	if pr := e.do(t, req{method: "GET", path: "/probe", bearer: b.body["access_token"].(string)}); pr.code != 200 {
		t.Fatalf("other user's session was revoked: %d", pr.code)
	}
}

func TestTermsRecorded(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	r := e.do(t, req{method: "POST", path: "/auth/terms", body: map[string]any{"version": "2026-10"}, bearer: at})
	if r.code != 204 {
		t.Fatalf("terms %d %s", r.code, r.raw)
	}
	var v string
	var ts *time.Time
	_ = super.QueryRow(context.Background(), `SELECT terms_version, terms_accepted_at FROM users WHERE id = $1`, u.id).Scan(&v, &ts)
	if v != "2026-10" || ts == nil {
		t.Fatalf("terms %q %v", v, ts)
	}
	var n int
	_ = super.QueryRow(context.Background(), `SELECT count(*) FROM audit_log WHERE action = 'auth.terms.accepted' AND actor_id = $1`, u.id).Scan(&n)
	if n != 1 {
		t.Fatalf("terms audit rows %d", n)
	}
	for _, body := range []any{map[string]any{"version": ""}, map[string]any{}, map[string]any{"version": strings.Repeat("v", 65)}} {
		if r := e.do(t, req{method: "POST", path: "/auth/terms", body: body, bearer: at}); r.code != 422 {
			t.Fatalf("bad version %v: %d", body, r.code)
		}
	}
	if r := e.do(t, req{method: "POST", path: "/auth/terms", body: map[string]any{"version": "x"}}); r.code != 401 {
		t.Fatalf("no token: %d", r.code)
	}
	// The next sign-in shows it.
	l2 := e.login(t, u.email, testPassword, newIP())
	if l2.body["user"].(map[string]any)["terms_accepted_at"] == nil {
		t.Fatal("terms_accepted_at not returned")
	}
}

func TestEveryAttemptAudited(t *testing.T) {
	e := newEnv(t)
	u := clinician(t)
	ip := newIP()
	var n0 int
	ctx := context.Background()
	_ = super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action LIKE 'auth.login.%'`).Scan(&n0)
	attempts := 0
	for i := 0; i < 6; i++ {
		e.login(t, u.email, "not-the-password-at-all", ip)
		attempts++
	}
	e.login(t, u.email, testPassword, newIP())
	e.login(t, "nobody."+uniq()+"@havana.example", testPassword, newIP())
	attempts += 2
	var n1 int
	_ = super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action LIKE 'auth.login.%'`).Scan(&n1)
	if n1-n0 != attempts {
		t.Fatalf("%d audit rows for %d attempts", n1-n0, attempts)
	}
	var leak int
	_ = super.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE payload::text ILIKE '%havana.example%' OR payload::text ILIKE '%password%'`).Scan(&leak)
	if leak != 0 {
		t.Fatalf("%d audit rows mention an email or password", leak)
	}
}

// --- R4: no credential in the logs after a failing request ------------------

func TestNoCredentialsInLogs(t *testing.T) {
	e := newEnv(t)
	u, l := loggedIn(t, e)
	at := l.body["access_token"].(string)
	rt := l.body["refresh_token"].(string)
	const badPW = "wrong-secret-password-9931"
	steps := map[string]req{
		"login wrong password": {method: "POST", path: "/auth/login", body: map[string]any{"username": u.email, "password": badPW}},
		"login unknown":        {method: "POST", path: "/auth/login", body: map[string]any{"username": "ghost." + u.email, "password": badPW}},
		"login malformed":      {method: "POST", path: "/auth/login", body: `{"username":"` + u.email + `","password":"` + badPW + `","x":1}`},
		"login platform":       {method: "POST", path: "/auth/login", platform: true, body: map[string]any{"username": u.email, "password": testPassword}},
		"refresh bad":          {method: "POST", path: "/auth/refresh", body: map[string]any{"refresh_token": rt + "x"}},
		"logout bad token":     {method: "POST", path: "/auth/logout", bearer: at + "x"},
		"sessions bad token":   {method: "GET", path: "/auth/sessions", bearer: at + "x"},
		"delete session 404":   {method: "DELETE", path: "/auth/sessions/" + uuid.NewString(), bearer: at},
		"revoke-all bad token": {method: "POST", path: "/auth/sessions/revoke-all", bearer: at + "x"},
		"terms bad":            {method: "POST", path: "/auth/terms", body: map[string]any{"version": ""}, bearer: at},
	}
	for name, s := range steps {
		e.logs = &syncBuffer{}
		e.svc.Log = logx.New(e.logs)
		r := e.do(t, s)
		if r.code < 400 {
			t.Fatalf("%s: expected a failure, got %d", name, r.code)
		}
		out := e.logs.String()
		for _, secret := range []string{u.email, strings.SplitN(u.email, "@", 2)[0], badPW, testPassword, at, rt, at[len(at)-20:], rt[:16]} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s: log contains a submitted credential or email:\n%s", name, out)
			}
		}
	}
	// Five failures then a lock, and a refresh reuse, also log nothing sensitive.
	e.logs = &syncBuffer{}
	e.svc.Log = logx.New(e.logs)
	ip := newIP()
	for i := 0; i < 6; i++ {
		e.login(t, u.email, badPW, ip)
	}
	r := e.refresh(t, rt)
	e.clock.Add(11 * time.Second)
	e.refresh(t, rt)
	out := e.logs.String()
	for _, secret := range []string{u.email, badPW, rt, r.body["refresh_token"].(string)} {
		if strings.Contains(out, secret) {
			t.Fatalf("lockout/reuse logs contain a credential:\n%s", out)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func jwtPayload(t *testing.T, tok string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(tok, ".")
	dec := func(s string) map[string]any {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	return dec(parts[0]), dec(parts[1])
}

// sameReply asserts two failure replies are indistinguishable: status,
// body without request_id, and headers without X-Request-ID.
func sameReply(t *testing.T, a, b resp) {
	t.Helper()
	if a.code != b.code {
		t.Fatalf("status %d vs %d", a.code, b.code)
	}
	strip := func(r resp) string {
		m := map[string]any{}
		for k, v := range r.body {
			if k != "request_id" {
				m[k] = v
			}
		}
		raw, _ := json.Marshal(m)
		return string(raw)
	}
	if strip(a) != strip(b) {
		t.Fatalf("bodies differ:\n%s\n%s", a.raw, b.raw)
	}
	hdr := func(r resp) string {
		var ks []string
		for k, v := range r.hdr {
			if k != "X-Request-Id" {
				ks = append(ks, k+"="+strings.Join(v, ","))
			}
		}
		sort.Strings(ks)
		return strings.Join(ks, ";")
	}
	if hdr(a) != hdr(b) {
		t.Fatalf("headers differ:\n%s\n%s", hdr(a), hdr(b))
	}
}

func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// assertAudit checks the request wrote exactly one row with action, whose
// payload doesn't contain email (when given).
func assertAudit(t *testing.T, r resp, action, email string) {
	t.Helper()
	rid := r.hdr.Get("X-Request-ID")
	rows, err := super.Query(context.Background(), `SELECT action, payload::text FROM audit_log WHERE request_id = $1`, rid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var a, p string
		if err := rows.Scan(&a, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, a)
		if strings.Contains(p, "@") || (email != "" && strings.Contains(p, email)) {
			t.Fatalf("audit payload has an email: %s", p)
		}
	}
	if len(got) != 1 || got[0] != action {
		t.Fatalf("audit for request: %v, want [%s]", got, action)
	}
}
