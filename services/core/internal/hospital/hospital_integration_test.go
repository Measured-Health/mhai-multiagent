//go:build integration

package hospital_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/db"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/hospital"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/testutil"
)

// The hospital admin routes on a migrated database, through the handlers
// with a verified principal in the context (the guard's role and two-step
// checks are authz's, pinned by TestHospitalRouteRules). Every name, email
// and text is synthetic and distinctive, so a leak is easy to spot.

var sharedSuper, sharedApp *pgxpool.Pool

func TestMain(m *testing.M) {
	code := func() int {
		ctx := context.Background()
		pg, stop, err := testutil.NewPostgres(ctx)
		if err != nil {
			panic(err)
		}
		defer stop()
		if err := db.MigrateUp(ctx, pg.URL); err != nil {
			panic(err)
		}
		super, err := db.Open(ctx, pg.URL)
		if err != nil {
			panic(err)
		}
		defer super.Close()
		app, err := testutil.AppLogin(ctx, pg, super)
		if err != nil {
			panic(err)
		}
		defer app.Close()
		sharedSuper, sharedApp = super, app
		return m.Run()
	}()
	os.Exit(code)
}

const (
	goodCode     = "246810"
	canaryReason = "SYNTH-CANARY-REASON-q7Zx because the dose looks off"
	canaryDx     = "SYNTH-CANARY-DX-k2Lw"
	otherEmail   = "synth-other-hospital-x9@h2.example.test"
	otherName    = "Synth Otherwise Mbeki-Z9"
)

type world struct {
	t               *testing.T
	h, h2           uuid.UUID
	admin, admin2   uuid.UUID
	ada, ben, other uuid.UUID
	logs            *bytes.Buffer
	mux             *http.ServeMux
	mu              sync.Mutex
	notified        []uuid.UUID
	codesChecked    int
}

func exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := sharedSuper.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q[:min(len(q), 60)], err)
	}
}

func scalar(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := sharedSuper.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q[:min(len(q), 60)], err)
	}
	return n
}

// person adds an account (superuser: exempt from 00030's rules, so test
// setup can place analysts directly).
func person(t *testing.T, h uuid.UUID, name, email string, mfa bool, roles ...string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := sharedSuper.QueryRow(context.Background(), `INSERT INTO users (hospital_id, email, password_hash, display_name, full_name, specialty)
		VALUES ($1, $2, 'synthetic-hash', $3, $3 || ' Full', 'Synthetic cardiology') RETURNING id`, h, email, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		exec(t, `INSERT INTO user_roles (user_id, role) VALUES ($1, $2)`, id, r)
	}
	exec(t, `INSERT INTO user_mfa (user_id, secret_enc, enabled) VALUES ($1, '\x00', $2)`, id, mfa)
	return id
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, h: uuid.New(), h2: uuid.New(), logs: &bytes.Buffer{}}
	tag := w.h.String()[:6]
	for i, h := range []uuid.UUID{w.h, w.h2} {
		exec(t, `INSERT INTO hospitals (id, name, short_code, status, time_zone) VALUES ($1, $2, $3, 'active', 'Africa/Lagos')`,
			h, "Synth General "+strconv.Itoa(i), "HA"+strconv.Itoa(i)+h.String()[:6])
		exec(t, `INSERT INTO hospital_settings (hospital_id, require_mfa_for_clinicians) VALUES ($1, true)`, h)
	}
	exec(t, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		SELECT NULL, 'on', 'SO-2026-0901', 'SO-2026-0901', 'SO-2026-0901'
		WHERE NOT EXISTS (SELECT 1 FROM analysis_switch WHERE hospital_id IS NULL)`)
	exec(t, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, 'on', 'SO-2026-0902', 'SO-2026-0902', 'SO-2026-0902')`, w.h)
	w.admin = person(t, w.h, "Synth Admin Adeyemi-"+tag, "admin-"+tag+"@h1.example.test", true, "hospital_admin")
	w.admin2 = person(t, w.h, "Synth Admin Two Bello-"+tag, "admin2-"+tag+"@h1.example.test", true, "hospital_admin")
	w.ada = person(t, w.h, "Synth Ada Okonkwo-"+tag, "ada-"+tag+"@h1.example.test", true, "clinician")
	w.ben = person(t, w.h, "Synth Ben Nwosu-"+tag, "ben-"+tag+"@h1.example.test", false, "clinician")
	w.other = person(t, w.h2, otherName, strings.Replace(otherEmail, "x9", tag, 1), true, "clinician", "hospital_admin")
	log := slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := hospital.WithStepUpCheck(hospital.Deps{Pool: sharedApp, Log: log,
		Notify: func(_ context.Context, _, p uuid.UUID) {
			w.mu.Lock()
			w.notified = append(w.notified, p)
			w.mu.Unlock()
		}},
		func(_ *http.Request, code, _ string) error {
			w.mu.Lock()
			w.codesChecked++
			w.mu.Unlock()
			if code != goodCode {
				return errors.New("wrong code")
			}
			return nil
		})
	w.mux = http.NewServeMux()
	for k, h := range hospital.Handlers(d) {
		method, path, _ := strings.Cut(k, " ")
		w.mux.Handle(method+" "+httpx.BasePath+path, h)
	}
	return w
}

// as serves a request as user u of hospital h.
func (w *world) as(h, u uuid.UUID, method, path, body string) (int, []byte) {
	w.t.Helper()
	r := httptest.NewRequest(method, httpx.BasePath+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	ctx := authz.WithPrincipal(r.Context(), authz.Principal{UserID: u, HospitalID: h,
		Roles: []string{authz.RoleHospitalAdmin}, MFA: true, SessionID: "s-" + u.String()})
	rec := httptest.NewRecorder()
	w.mux.ServeHTTP(rec, r.WithContext(ctx))
	return rec.Code, rec.Body.Bytes()
}

func (w *world) adm(method, path, body string) (int, map[string]any) {
	w.t.Helper()
	code, raw := w.as(w.h, w.admin, method, path, body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return code, out
}

func (w *world) version(t *testing.T, id uuid.UUID) int {
	return scalar(t, `SELECT staff_version FROM users WHERE id = $1`, id)
}

func (w *world) roles(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	var rs []string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT ARRAY(SELECT role FROM user_roles WHERE user_id = $1 ORDER BY role)`, id).Scan(&rs); err != nil {
		t.Fatal(err)
	}
	return rs
}

func audits(t *testing.T, h uuid.UUID, action string) int {
	return scalar(t, `SELECT count(*) FROM audit_log WHERE hospital_id = $1 AND action = $2`, h, action)
}

func patch(v int, extra string) string {
	return `{"version": ` + strconv.Itoa(v) + extra + `}`
}

func noLeaks(t *testing.T, w *world, body []byte) {
	t.Helper()
	for _, s := range []string{otherName, "h2.example.test", canaryReason, canaryDx, "synthetic-hash"} {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("reply carries %q", s)
		}
	}
	for _, s := range []string{"Synth Ada", "Synth Ben", "Synth Admin", "@h1.example.test", canaryReason, canaryDx} {
		if strings.Contains(w.logs.String(), s) {
			t.Errorf("logs carry %q", s)
		}
	}
}

// GET /hospital/staff: everyone at the caller's hospital, nobody else's.
func TestStaffListIsolation(t *testing.T) {
	w := newWorld(t)
	code, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/staff", "")
	if code != http.StatusOK {
		t.Fatalf("list %d %s", code, raw)
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, s := range list {
		byID[s["id"].(string)] = s
	}
	if len(list) != 4 || byID[w.other.String()] != nil {
		t.Fatalf("list %d entries, other hospital %v", len(list), byID[w.other.String()] != nil)
	}
	me, ada, ben := byID[w.admin.String()], byID[w.ada.String()], byID[w.ben.String()]
	if me["is_you"] != true || ada["is_you"] != false || me["status"] != "active" || me["sign_in"] != "password_mfa" {
		t.Errorf("me %v", me)
	}
	if ada["can_review"] != true || me["can_review"] != false || ada["version"] != float64(1) {
		t.Errorf("ada %v", ada)
	}
	if issues, _ := ben["issues"].([]any); len(issues) != 1 || issues[0] != "mfa_not_set_up" || ben["sign_in"] != "password" {
		t.Errorf("ben %v", ben)
	}
	noLeaks(t, w, raw)
	// Another hospital's admin sees theirs only.
	_, raw = w.as(w.h2, w.other, http.MethodGet, "/hospital/staff", "")
	if bytes.Contains(raw, []byte("Synth Ada")) {
		t.Fatal("hospital 2 sees hospital 1's staff")
	}
}

// Roles: guards before the code; the code; the change and its audit entry;
// another hospital's person is 404.
func TestStaffPatchRoles(t *testing.T) {
	w := newWorld(t)
	path := "/hospital/staff/" + w.ada.String()
	// Stale version: conflict, without asking for (or spending) a code.
	if code, out := w.adm(http.MethodPatch, path, patch(9, `, "roles": ["clinician", "hospital_admin"]`)); code != 409 || out["code"] != "conflict" {
		t.Fatalf("stale: %d %v", code, out)
	}
	// Self: dropping your own hospital_admin.
	if code, out := w.adm(http.MethodPatch, "/hospital/staff/"+w.admin.String(), patch(1, `, "roles": ["clinician"]`)); code != 403 || out["code"] != "self_change" {
		t.Fatalf("self: %d %v", code, out)
	}
	// Another hospital's person: 404.
	if code, _ := w.adm(http.MethodPatch, "/hospital/staff/"+w.other.String(), patch(1, `, "roles": ["clinician"]`)); code != 404 {
		t.Fatalf("other hospital: %d", code)
	}
	if w.codesChecked != 0 {
		t.Fatalf("a refused request checked %d codes", w.codesChecked)
	}
	// Bad body.
	for _, b := range []string{patch(1, `, "roles": ["platform_admin"]`), patch(1, `, "roles": []`), `{"roles": ["clinician"]}`,
		patch(1, `, "specialty": "Cardio 1999"`), patch(1, `, "review_specialties": ["cardiology"]`), patch(1, `, "extra": 1`)} {
		if code, _ := w.adm(http.MethodPatch, path, b); code != 422 {
			t.Errorf("%s: %d", b, code)
		}
	}
	// No code, then a wrong code: refused, nothing changed.
	if code, out := w.adm(http.MethodPatch, path, patch(1, `, "roles": ["clinician", "hospital_admin"]`)); code != 403 || out["code"] != "mfa_required" {
		t.Fatalf("no code: %d %v", code, out)
	}
	if code, out := w.adm(http.MethodPatch, path, patch(1, `, "roles": ["clinician", "hospital_admin"], "mfa_code": "000000"`)); code != 403 || out["code"] != "mfa_invalid" {
		t.Fatalf("wrong code: %d %v", code, out)
	}
	if !slices.Equal(w.roles(t, w.ada), []string{"clinician"}) || audits(t, w.h, "staff.roles_changed") != 0 {
		t.Fatal("a refused edit changed something")
	}
	code, out := w.adm(http.MethodPatch, path, patch(1, `, "roles": ["clinician", "hospital_admin"], "specialty": "Synthetic nephrology", "mfa_code": "`+goodCode+`"`))
	if code != 200 || out["version"] != float64(2) || out["specialty"] != "Synthetic nephrology" {
		t.Fatalf("grant admin: %d %v", code, out)
	}
	if !slices.Equal(w.roles(t, w.ada), []string{"clinician", "hospital_admin"}) {
		t.Fatalf("roles %v", w.roles(t, w.ada))
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE hospital_id = $1 AND action = 'staff.roles_changed' AND actor_id = $2
		AND entity = 'user' AND entity_id = $3 AND payload = '{"added": ["hospital_admin"], "removed": [], "self": false}'::jsonb`,
		w.h, w.admin, w.ada.String()); n != 1 {
		t.Errorf("roles_changed entries %d", n)
	}
	if audits(t, w.h, "staff.specialty_changed") != 1 {
		t.Error("specialty change not audited")
	}
	// A self role add: allowed, flagged.
	code, out = w.adm(http.MethodPatch, "/hospital/staff/"+w.admin.String(), patch(1, `, "roles": ["hospital_admin", "clinician"], "mfa_code": "`+goodCode+`"`))
	if code != 200 || out["is_you"] != true {
		t.Fatalf("self clinician: %d %v", code, out)
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE action = 'staff.roles_changed' AND entity_id = $1 AND (payload->>'self')::boolean`,
		w.admin.String()); n != 1 {
		t.Errorf("self role change entries %d", n)
	}
	// Demote: ada had hospital_admin; dropping it with another admin left.
	code, _ = w.adm(http.MethodPatch, path, patch(2, `, "roles": ["clinician"], "mfa_code": "`+goodCode+`"`))
	if code != 200 || !slices.Equal(w.roles(t, w.ada), []string{"clinician"}) {
		t.Fatalf("demote %d %v", code, w.roles(t, w.ada))
	}
	// A no-op edit asks for no code and writes nothing.
	before := audits(t, w.h, "staff.roles_changed")
	if code, _ := w.adm(http.MethodPatch, path, patch(3, `, "roles": ["clinician"]`)); code != 200 || audits(t, w.h, "staff.roles_changed") != before {
		t.Fatalf("no-op %d", code)
	}
	noLeaks(t, w, nil)
}

// T26 (route), Analysis Task 17: Analyst through 00030's functions, each
// refusal mapped; without a code 403 mfa_required; a grant notifies once
// after commit.
func TestT26RouteRefusals(t *testing.T) {
	w := newWorld(t)
	grant := func(id uuid.UUID, v int, code string) (int, map[string]any) {
		extra := `, "roles": ["clinician", "analyst"]`
		if code != "" {
			extra += `, "mfa_code": "` + code + `"`
		}
		return w.adm(http.MethodPatch, "/hospital/staff/"+id.String(), patch(v, extra))
	}
	if code, out := grant(w.ada, 1, ""); code != 403 || out["code"] != "mfa_required" {
		t.Fatalf("no code: %d %v", code, out)
	}
	if code, out := grant(w.ada, 1, "135799"); code != 403 || out["code"] != "mfa_invalid" {
		t.Fatalf("wrong code: %d %v", code, out)
	}
	// Ben has no two-step: the database refuses.
	if code, out := grant(w.ben, 1, goodCode); code != 422 || out["code"] != "not_eligible" {
		t.Fatalf("no two-step: %d %v", code, out)
	}
	// Analyst without clinician: refused before anything is read.
	if code, out := w.adm(http.MethodPatch, "/hospital/staff/"+w.ada.String(), patch(1, `, "roles": ["analyst"]`)); code != 422 || out["code"] != "not_eligible" {
		t.Fatalf("analyst alone: %d %v", code, out)
	}
	// Yourself: self_change.
	exec(t, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'clinician')`, w.admin)
	if code, out := w.adm(http.MethodPatch, "/hospital/staff/"+w.admin.String(), patch(1, `, "roles": ["clinician", "analyst", "hospital_admin"], "mfa_code": "`+goodCode+`"`)); code != 403 || out["code"] != "self_change" {
		t.Fatalf("self: %d %v", code, out)
	}
	// The switch off.
	exec(t, `UPDATE analysis_switch SET position = 'off' WHERE hospital_id = $1`, w.h)
	if code, out := grant(w.ada, 1, goodCode); code != 409 || out["code"] != "analysis_off" {
		t.Fatalf("switch off: %d %v", code, out)
	}
	exec(t, `UPDATE analysis_switch SET position = 'on' WHERE hospital_id = $1`, w.h)
	if len(w.notified) != 0 || slices.Contains(w.roles(t, w.ada), "analyst") || audits(t, w.h, "analyst.granted") != 0 {
		t.Fatal("a refused grant changed something")
	}
	// The grant.
	code, out := grant(w.ada, 1, goodCode)
	if code != 200 || !slices.Contains(w.roles(t, w.ada), "analyst") {
		t.Fatalf("grant: %d %v", code, out)
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE hospital_id = $1 AND action = 'analyst.granted' AND entity_id = $2
		AND actor_id = $3 AND payload->>'via' = 'app'`, w.h, w.ada.String(), w.admin); n != 1 {
		t.Fatalf("analyst.granted entries %d", n)
	}
	if !slices.Equal(w.notified, []uuid.UUID{w.ada}) {
		t.Fatalf("notified %v", w.notified)
	}
	// The cap: ten analysts (placed by the superuser, exempt), then an 11th.
	for i := 0; i < 9; i++ {
		person(t, w.h, "Synth Analyst Filler-"+strconv.Itoa(i), uuid.NewString()[:8]+"@h1.example.test", true, "clinician", "analyst")
	}
	exec(t, `UPDATE user_mfa SET enabled = true WHERE user_id = $1`, w.ben)
	if code, out := grant(w.ben, 1, goodCode); code != 409 || out["code"] != "analyst_cap" {
		t.Fatalf("cap: %d %v", code, out)
	}
	if len(w.notified) != 1 {
		t.Fatalf("a refused grant notified: %v", w.notified)
	}
	noLeaks(t, w, nil)
}

// T13: removing Analyst through the route takes it from the database at
// once (the patient-level routes re-read it, analysis/routes T13), and
// removing Clinician takes Analyst with it, audited.
func TestRemoveAnalystEndsPatientAccessAtOnce(t *testing.T) {
	w := newWorld(t)
	path := "/hospital/staff/" + w.ada.String()
	if code, out := w.adm(http.MethodPatch, path, patch(1, `, "roles": ["clinician", "analyst"], "mfa_code": "`+goodCode+`"`)); code != 200 {
		t.Fatalf("grant %d %v", code, out)
	}
	code, out := w.adm(http.MethodPatch, path, patch(2, `, "roles": ["clinician"], "mfa_code": "`+goodCode+`"`))
	if code != 200 {
		t.Fatalf("remove %d %v", code, out)
	}
	// What analysis/routes' stillAllowed reads, right after the reply.
	var roles []string
	if err := sharedSuper.QueryRow(context.Background(), `SELECT ARRAY(SELECT r.role FROM user_roles r WHERE r.user_id = $1)`, w.ada).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(roles, "analyst") {
		t.Fatalf("analyst still held: %v", roles)
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE action = 'analyst.removed' AND entity_id = $1 AND payload->>'reason' = 'removed'`,
		w.ada.String()); n != 1 {
		t.Fatalf("analyst.removed entries %d", n)
	}
	// Grant again, then take Clinician (and so Analyst) away in one edit.
	if code, _ := w.adm(http.MethodPatch, path, patch(3, `, "roles": ["clinician", "analyst"], "mfa_code": "`+goodCode+`"`)); code != 200 {
		t.Fatalf("regrant %d", code)
	}
	if code, out := w.adm(http.MethodPatch, path, patch(4, `, "roles": ["hospital_admin"], "mfa_code": "`+goodCode+`"`)); code != 200 {
		t.Fatalf("clinician off %d %v", code, out)
	}
	if !slices.Equal(w.roles(t, w.ada), []string{"hospital_admin"}) {
		t.Fatalf("roles %v", w.roles(t, w.ada))
	}
	if n := scalar(t, `SELECT count(*) FROM analyst_changes c WHERE c.user_id = $1 AND NOT EXISTS (SELECT 1 FROM audit_log a
		WHERE a.payload->>'change_id' = c.id::text)`, w.ada); n != 0 {
		t.Fatalf("%d analyst changes unaudited", n)
	}
}

// Deactivate and reactivate: the step-up after the guards, sessions
// revoked, audited; yourself and a wrong status refused.
func TestDeactivateReactivate(t *testing.T) {
	w := newWorld(t)
	exec(t, `INSERT INTO sessions (user_id) VALUES ($1), ($1)`, w.ada)
	exec(t, `UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND id = (SELECT id FROM sessions WHERE user_id = $1 LIMIT 1)`, w.ada)
	exec(t, `INSERT INTO trusted_devices (user_id, token_hash) VALUES ($1, $2)`, w.ada, "synth-device-hash-"+w.ada.String())
	exec(t, `INSERT INTO user_roles (user_id, role) VALUES ($1, 'analyst')`, w.ada) // superuser: exempt from 00030
	de := "/hospital/staff/" + w.ada.String() + "/deactivate"
	if code, out := w.adm(http.MethodPost, "/hospital/staff/"+w.admin.String()+"/deactivate", ""); code != 403 || out["code"] != "self_change" {
		t.Fatalf("self %d %v", code, out)
	}
	if code, out := w.adm(http.MethodPost, "/hospital/staff/"+w.ada.String()+"/reactivate", ""); code != 409 || out["code"] != "conflict" {
		t.Fatalf("reactivate active %d %v", code, out)
	}
	if code, out := w.adm(http.MethodPost, "/hospital/staff/"+w.other.String()+"/deactivate", `{"mfa_code": "`+goodCode+`"}`); code != 404 {
		t.Fatalf("other hospital %d %v", code, out)
	}
	if code, out := w.adm(http.MethodPost, de, ""); code != 403 || out["code"] != "mfa_required" {
		t.Fatalf("no code %d %v", code, out)
	}
	code, out := w.adm(http.MethodPost, de, `{"mfa_code": "`+goodCode+`"}`)
	if code != 200 || out["status"] != "deactivated" || out["can_review"] != false {
		t.Fatalf("deactivate %d %v", code, out)
	}
	if n := scalar(t, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, w.ada); n != 0 {
		t.Fatalf("%d sessions live", n)
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE action = 'staff.deactivated' AND entity_id = $1
		AND payload = '{"sessions_revoked": 1}'::jsonb`, w.ada.String()); n != 1 {
		t.Fatalf("deactivated entries %d", n)
	}
	if n := scalar(t, `SELECT count(*) FROM trusted_devices WHERE user_id = $1`, w.ada); n != 0 {
		t.Errorf("%d trusted devices kept", n)
	}
	if slices.Contains(w.roles(t, w.ada), "analyst") || scalar(t, `SELECT count(*) FROM audit_log WHERE action = 'analyst.removed'
		AND entity_id = $1`, w.ada.String()) != 1 {
		t.Errorf("analyst kept or unaudited: %v", w.roles(t, w.ada))
	}
	if code, _ := w.adm(http.MethodPost, de, `{"mfa_code": "`+goodCode+`"}`); code != 409 {
		t.Fatalf("again %d", code)
	}
	code, out = w.adm(http.MethodPost, "/hospital/staff/"+w.ada.String()+"/reactivate", `{"mfa_code": "`+goodCode+`"}`)
	if code != 200 || out["status"] != "active" || audits(t, w.h, "staff.reactivated") != 1 {
		t.Fatalf("reactivate %d %v", code, out)
	}
	noLeaks(t, w, nil)
}

// Concurrent demotions of the hospital's only two admins by each other:
// the advisory lock keeps one.
func TestLastAdminUnderConcurrency(t *testing.T) {
	w := newWorld(t)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, pair := range [][2]uuid.UUID{{w.admin, w.admin2}, {w.admin2, w.admin}} {
		wg.Add(1)
		go func(i int, by, target uuid.UUID) {
			defer wg.Done()
			codes[i], _ = w.as(w.h, by, http.MethodPost, "/hospital/staff/"+target.String()+"/deactivate", `{"mfa_code": "`+goodCode+`"}`)
		}(i, pair[0], pair[1])
	}
	wg.Wait()
	if n := scalar(t, `SELECT count(*) FROM users u JOIN user_roles r ON r.user_id = u.id AND r.role = 'hospital_admin'
		WHERE u.hospital_id = $1 AND u.is_active`, w.h); n < 1 {
		t.Fatalf("no active admin left (codes %v)", codes)
	}
}

func TestSettings(t *testing.T) {
	w := newWorld(t)
	code, out := w.adm(http.MethodGet, "/hospital/settings", "")
	if code != 200 || out["version"] != float64(1) || out["require_mfa_for_clinicians"] != true || out["hospital_name"] != "Synth General 0" {
		t.Fatalf("get %d %v", code, out)
	}
	if sso, _ := out["sso"].(map[string]any); sso["status"] != "not_set_up" {
		t.Errorf("sso %v", out["sso"])
	}
	if sp, _ := out["specialties"].([]any); len(sp) != 1 || sp[0] != "Synthetic cardiology" {
		t.Errorf("specialties %v", out["specialties"])
	}
	for _, b := range []string{`{"version": 1, "review_overdue_hours": 7}`, `{"version": 1, "idle_timeout_minutes": 600}`,
		`{"version": 1, "reviewers_share_specialty": true}`, `{"review_overdue_hours": 8}`, `{"version": 1, "sso": {}}`} {
		if code, _ := w.adm(http.MethodPatch, "/hospital/settings", b); code != 422 {
			t.Errorf("%s: %d", b, code)
		}
	}
	// Tightening: no code.
	code, out = w.adm(http.MethodPatch, "/hospital/settings", `{"version": 1, "review_overdue_hours": 8, "idle_timeout_minutes": 10}`)
	if code != 200 || out["version"] != float64(2) || out["review_overdue_hours"] != float64(8) {
		t.Fatalf("tighten %d %v", code, out)
	}
	if code, out := w.adm(http.MethodPatch, "/hospital/settings", `{"version": 1, "review_overdue_hours": 2}`); code != 409 || out["code"] != "conflict" {
		t.Fatalf("stale %d %v", code, out)
	}
	// Loosening: the code, after the version.
	if code, out := w.adm(http.MethodPatch, "/hospital/settings", `{"version": 2, "require_mfa_for_clinicians": false}`); code != 403 || out["code"] != "mfa_required" {
		t.Fatalf("loosen without code %d %v", code, out)
	}
	code, out = w.adm(http.MethodPatch, "/hospital/settings", `{"version": 2, "require_mfa_for_clinicians": false, "allow_trust_device": true, "mfa_code": "`+goodCode+`"}`)
	if code != 200 || out["require_mfa_for_clinicians"] != false || out["allow_trust_device"] != true {
		t.Fatalf("loosen %d %v", code, out)
	}
	if n := scalar(t, `SELECT count(*) FROM audit_log WHERE hospital_id = $1 AND action = 'hospital_settings.changed'
		AND payload->>'loosened' = 'true' AND payload->'fields' = '["require_mfa_for_clinicians", "allow_trust_device"]'::jsonb`, w.h); n != 1 {
		t.Errorf("loosened entries %d", n)
	}
	// Hospital 2 is untouched.
	if n := scalar(t, `SELECT version FROM hospital_settings WHERE hospital_id = $1`, w.h2); n != 1 {
		t.Errorf("hospital 2's settings moved to %d", n)
	}
}

// query adds a consultation of hospital h by u.
func query(t *testing.T, h, u uuid.UUID, status, decision, category string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	exec(t, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, category, status, progress, created_by)
		VALUES ($1, $2, 'SYNTH-CANARY-QUESTION-p4', 'routine', NULLIF($3, ''), $4, 1, $5)`, id, h, category, status, u)
	if decision != "" {
		exec(t, `INSERT INTO clinical_decisions (id, hospital_id, query_id, hga_decision) VALUES ($1, $2, $3, $4)`, uuid.New(), h, id, decision)
	}
	return id
}

// correction adds a waiting correction of hospital h by u on query q.
func correction(t *testing.T, h, u, q uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	exec(t, `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status, reason, corrected_diagnosis, created_at)
		VALUES ($1, $2, $3, $4, 'correct', 'pending_review', $5, $6, now() - interval '3 hours')`, id, h, q, u, canaryReason, canaryDx)
	return id
}

// decide approves correction id as reviewer r.
func decide(t *testing.T, id, r uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	err := pgx.BeginFunc(ctx, sharedSuper, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('mhai.user_id', $1, true)`, r.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE feedback_events SET status = 'approved', decided_by = $2, decided_at = now() - interval '1 hour' WHERE id = $1`, id, r)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUsage(t *testing.T) {
	w := newWorld(t)
	q1 := query(t, w.h, w.ada, "completed", "PASS", "Synthetic cardiology")
	query(t, w.h, w.ada, "completed", "REGENERATE", "Synthetic cardiology")
	query(t, w.h, w.ben, "failed", "", "")
	query(t, w.h2, w.other, "completed", "PASS", "Synthetic oncology")
	exec(t, `INSERT INTO feedback_events (id, hospital_id, query_id, submitted_by, action, status) VALUES ($1, $2, $3, $4, 'validate', 'recorded')`,
		uuid.New(), w.h, q1, w.ada)
	q2 := query(t, w.h, w.ben, "completed", "ESCALATE", "")
	decide(t, correction(t, w.h, w.ben, q2), w.ada)
	code, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/usage?period=this_month", "")
	if code != 200 {
		t.Fatalf("usage %d %s", code, raw)
	}
	var u map[string]any
	_ = json.Unmarshal(raw, &u)
	if u["consultations"] != float64(4) || u["active_clinicians"] != float64(2) || u["agreement_rate"] != 0.5 {
		t.Errorf("counts %v", u)
	}
	if mh, _ := u["median_review_hours"].(float64); mh < 1.99 || mh > 2.01 {
		t.Errorf("median %v", u["median_review_hours"])
	}
	conf, _ := u["confidence"].(map[string]any)
	if conf["high"] != float64(1) || conf["caution"] != float64(1) || conf["review"] != float64(1) || conf["failed"] != float64(1) {
		t.Errorf("confidence %v", conf)
	}
	if bs, _ := u["by_specialty"].([]any); len(bs) != 1 {
		t.Errorf("by_specialty %v", u["by_specialty"])
	}
	if u["alerts"] != nil {
		t.Errorf("alerts with no EMR source: %v", u["alerts"])
	}
	if bytes.Contains(raw, []byte("oncology")) || bytes.Contains(raw, []byte("CANARY")) {
		t.Errorf("usage leaks: %s", raw)
	}
	for _, q := range []string{"", "?period=this_year", "?period=this_month&period=last_month", "?period=this_month&x=1"} {
		if code, _ := w.as(w.h, w.admin, http.MethodGet, "/hospital/usage"+q, ""); code != 422 {
			t.Errorf("%q: %d", q, code)
		}
	}
	// last_month: nothing yet.
	_, raw = w.as(w.h, w.admin, http.MethodGet, "/hospital/usage?period=last_month", "")
	_ = json.Unmarshal(raw, &u)
	if u["consultations"] != float64(0) || u["agreement_rate"] != nil {
		t.Errorf("last month %v", u)
	}
}

func TestActivity(t *testing.T) {
	w := newWorld(t)
	path := "/hospital/staff/" + w.ada.String()
	if code, _ := w.adm(http.MethodPatch, path, patch(1, `, "roles": ["clinician", "hospital_admin"], "mfa_code": "`+goodCode+`"`)); code != 200 {
		t.Fatal(code)
	}
	if code, _ := w.adm(http.MethodPatch, "/hospital/staff/"+w.admin.String(), patch(1, `, "roles": ["clinician", "hospital_admin"], "mfa_code": "`+goodCode+`"`)); code != 200 {
		t.Fatal(code)
	}
	// Hospital 2's own change.
	o2 := person(t, w.h2, "Synth Other Clinician-Q1", uuid.NewString()[:8]+"@h2.example.test", true, "clinician")
	if code, _ := w.as(w.h2, w.other, http.MethodPost, "/hospital/staff/"+o2.String()+"/deactivate", `{"mfa_code": "`+goodCode+`"}`); code != 200 {
		t.Fatal(code)
	}
	code, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/activity", "")
	if code != 200 {
		t.Fatalf("activity %d %s", code, raw)
	}
	var page struct {
		Events []map[string]any `json:"events"`
		Next   *string          `json:"next_cursor"`
	}
	_ = json.Unmarshal(raw, &page)
	if len(page.Events) != 2 || page.Next != nil {
		t.Fatalf("events %s", raw)
	}
	self, other := page.Events[0], page.Events[1]
	if self["kind"] != "self_role_change" || self["type"] != "security" || self["flag"] != "alert" {
		t.Errorf("self %v", self)
	}
	if ra, _ := self["roles_added"].([]any); len(ra) != 1 || ra[0] != "clinician" {
		t.Errorf("roles_added %v", self["roles_added"])
	}
	if other["kind"] != "roles_changed" || other["type"] != "admin" || other["flag"] != nil {
		t.Errorf("other %v", other)
	}
	if tg, _ := other["target"].(map[string]any); !strings.HasPrefix(tg["display_name"].(string), "Synth Ada") {
		t.Errorf("target %v", other["target"])
	}
	if bytes.Contains(raw, []byte("Synth Other")) || bytes.Contains(raw, []byte(w.ada.String())) {
		t.Errorf("activity leaks: %s", raw)
	}
	get := func(q string) (int, int, *string) {
		code, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/activity?"+q, "")
		var p struct {
			Events []map[string]any `json:"events"`
			Next   *string          `json:"next_cursor"`
		}
		_ = json.Unmarshal(raw, &p)
		return code, len(p.Events), p.Next
	}
	if _, n, _ := get("flagged=true"); n != 1 {
		t.Errorf("flagged %d", n)
	}
	if _, n, _ := get("type=admin"); n != 1 {
		t.Errorf("admin %d", n)
	}
	if _, n, _ := get("type=review"); n != 0 {
		t.Errorf("review %d", n)
	}
	if _, n, _ := get("person=" + "ada%20okonkwo"); n != 1 {
		t.Errorf("person %d", n)
	}
	if _, n, _ := get("person=%25"); n != 0 {
		t.Errorf("a wildcard person matched %d", n)
	}
	if _, n, _ := get("staff_id=" + w.ada.String()); n != 1 {
		t.Errorf("staff_id %d", n)
	}
	_, n, next := get("limit=1")
	if n != 1 || next == nil {
		t.Fatalf("page 1: %d %v", n, next)
	}
	if _, n, next := get("limit=1&cursor=" + *next); n != 1 || next != nil {
		t.Errorf("page 2: %d %v", n, next)
	}
	if code, _, _ := get("type=bogus"); code != 422 {
		t.Errorf("bad type %d", code)
	}
}

func TestReviewOversight(t *testing.T) {
	w := newWorld(t)
	q := query(t, w.h, w.ada, "completed", "PASS", "Synthetic cardiology")
	c := correction(t, w.h, w.ada, q)
	qo := query(t, w.h2, w.other, "completed", "PASS", "")
	co := correction(t, w.h2, w.other, qo)
	code, raw := w.as(w.h, w.admin, http.MethodGet, "/hospital/review-oversight", "")
	if code != 200 {
		t.Fatalf("oversight %d %s", code, raw)
	}
	var o map[string]any
	_ = json.Unmarshal(raw, &o)
	items, _ := o["items"].([]any)
	if o["waiting"] != float64(1) || o["overdue_after_hours"] != float64(4) || len(items) != 1 {
		t.Fatalf("oversight %s", raw)
	}
	it := items[0].(map[string]any)
	if it["id"] != c.String() || it["ref"] != hospital.Ref(c) || it["specialty"] != "Synthetic cardiology" ||
		it["eligible_reviewers"] != float64(1) || it["assigned_to"] != nil {
		t.Errorf("item %v", it)
	}
	if bytes.Contains(raw, []byte("CANARY")) || bytes.Contains(raw, []byte(co.String())) {
		t.Errorf("oversight leaks: %s", raw)
	}
	code, raw = w.as(w.h, w.admin, http.MethodGet, "/hospital/corrections/"+c.String()+"/eligible-reviewers", "")
	var rs []map[string]any
	_ = json.Unmarshal(raw, &rs)
	if code != 200 || len(rs) != 1 || rs[0]["id"] != w.ben.String() {
		t.Fatalf("eligible %d %s", code, raw)
	}
	if code, _ := w.as(w.h, w.admin, http.MethodGet, "/hospital/corrections/"+co.String()+"/eligible-reviewers", ""); code != 404 {
		t.Errorf("other hospital's correction %d", code)
	}
	decide(t, c, w.ben)
	if code, _ := w.as(w.h, w.admin, http.MethodGet, "/hospital/corrections/"+c.String()+"/eligible-reviewers", ""); code != 409 {
		t.Errorf("decided %d", code)
	}
	_, raw = w.as(w.h, w.admin, http.MethodGet, "/hospital/review-oversight", "")
	_ = json.Unmarshal(raw, &o)
	if o["waiting"] != float64(0) || o["decided_this_week"] != float64(1) {
		t.Errorf("after decision %s", raw)
	}
}

// The routes core has no feature behind answer 501 not_implemented.
func TestUnsupportedRoutes(t *testing.T) {
	w := newWorld(t)
	for _, u := range hospital.Unsupported {
		p := strings.ReplaceAll(u.Pattern, "{id}", w.ada.String())
		if code, out := w.adm(u.Method, p, `{"emails": ["synth@h1.example.test"], "roles": ["clinician"], "specialty": null}`); code != 501 || out["code"] != "not_implemented" {
			t.Errorf("%s %s: %d %v", u.Method, p, code, out)
		}
	}
}
