//go:build integration && test

package auth_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// clinicalToken signs in (by minting) an account at a new hospital with
// roles, proven by a code so no two-step policy gets in the way.
func (e *env) clinicalToken(t *testing.T, roles ...string) (string, uuid.UUID) {
	t.Helper()
	hid := seedHospital(t, hospitalOpts{})
	u := seedUser(t, hid, true, roles...)
	at, _ := e.mint(t, u, hid.String(), roles, true, "pwd", "otp")
	return at, hid
}

func (e *env) platformToken(t *testing.T, role string) string {
	t.Helper()
	u := seedUser(t, uuid.Nil, true, role)
	at, _ := e.mint(t, u, "", []string{role}, true, "pwd", "otp")
	return at
}

func TestRoleRequired(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		roles       []string
		clin, admin int
	}{
		{[]string{"clinician"}, 200, 403},
		{[]string{"analyst"}, 403, 403},
		{[]string{"hospital_admin"}, 403, 200},
		// Roles combine on clinical accounts.
		{[]string{"clinician", "hospital_admin"}, 200, 200},
		{[]string{"clinician", "hospital_admin", "analyst"}, 200, 200},
	} {
		at, _ := e.clinicalToken(t, c.roles...)
		if r := e.do(t, req{method: "GET", path: "/_test/clinical", bearer: at}); r.code != c.clin {
			t.Errorf("%v clinical: %d %s", c.roles, r.code, r.raw)
		} else if r.code == 403 && r.body["code"] != "forbidden" {
			t.Errorf("%v clinical code %s", c.roles, r.raw)
		}
		if r := e.do(t, req{method: "GET", path: "/_test/admin", bearer: at}); r.code != c.admin {
			t.Errorf("%v admin: %d %s", c.roles, r.code, r.raw)
		}
	}
	// No token at all.
	mustCode(t, "anonymous", e.do(t, req{method: "GET", path: "/_test/clinical"}), 401, "unauthenticated")
}

func TestHospitalAdminNotClinician403(t *testing.T) {
	e := newEnv(t)
	at, _ := e.clinicalToken(t, "hospital_admin")
	mustCode(t, "admin on clinical", e.do(t, req{method: "GET", path: "/_test/clinical", bearer: at}), 403, "forbidden")
	at, _ = e.clinicalToken(t, "hospital_admin", "analyst")
	mustCode(t, "admin+analyst on clinical", e.do(t, req{method: "GET", path: "/_test/clinical", bearer: at}), 403, "forbidden")
}

func TestPlatformAudienceRequired(t *testing.T) {
	e := newEnv(t)
	pa := e.platformToken(t, "platform_admin")
	clin, _ := e.clinicalToken(t, "clinician", "hospital_admin")
	for name, c := range map[string]struct {
		bearer   string
		platform bool
		want     int
		code     string
	}{
		"platform account, platform audience": {pa, true, 200, ""},
		"platform account, no audience":       {pa, false, 403, "not_platform"},
		"clinical account, platform audience": {clin, true, 403, "not_platform"},
		"clinical account, no audience":       {clin, false, 403, "not_platform"},
	} {
		r := e.do(t, req{method: "GET", path: "/_test/platform", bearer: c.bearer, platform: c.platform})
		if r.code != c.want || (c.code != "" && r.body["code"] != c.code) {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
	// A platform account on a clinical route is forbidden.
	r := e.do(t, req{method: "GET", path: "/_test/clinical", bearer: pa, platform: true})
	if r.code != 403 {
		t.Errorf("platform on clinical: %d %s", r.code, r.raw)
	}
}

func TestAuditorWriteForbidden(t *testing.T) {
	e := newEnv(t)
	aud := e.platformToken(t, "auditor")
	if r := e.do(t, req{method: "GET", path: "/_test/platform", bearer: aud, platform: true}); r.code != 200 {
		t.Fatalf("auditor read: %d %s", r.code, r.raw)
	}
	r := e.do(t, req{method: "POST", path: "/_test/platform", bearer: aud, platform: true})
	if r.code != 403 || r.raw != `{"code":"forbidden"}` {
		t.Fatalf("auditor write: %d %s", r.code, r.raw)
	}
	pa := e.platformToken(t, "platform_admin")
	if r := e.do(t, req{method: "POST", path: "/_test/platform", bearer: pa, platform: true}); r.code != 204 {
		t.Fatalf("platform admin write: %d %s", r.code, r.raw)
	}
	// Auditors still use their own account routes (not Writes).
	if r := e.do(t, req{method: "GET", path: "/auth/sessions", bearer: aud, platform: true}); r.code != 200 {
		t.Fatalf("auditor sessions: %d %s", r.code, r.raw)
	}
}

// Another hospital's row is 404, never 403: the fixture's query has no
// hospital filter, and row-level security under authz.Tx hides the row.
func TestOtherHospitalResource404(t *testing.T) {
	e := newEnv(t)
	atA, hidA := e.clinicalToken(t, "clinician")
	_, hidB := e.clinicalToken(t, "clinician")
	ctx := context.Background()
	rowA, rowB := uuid.New(), uuid.New()
	for id, hid := range map[uuid.UUID]uuid.UUID{rowA: hidA, rowB: hidB} {
		if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency)
			VALUES ($1, $2, 'Synthetic question', 'routine')`, id, hid); err != nil {
			t.Fatal(err)
		}
	}
	if r := e.do(t, req{method: "GET", path: "/_test/clinical-queries/" + rowA.String(), bearer: atA}); r.code != 200 || r.body["id"] != rowA.String() {
		t.Fatalf("own row: %d %s", r.code, r.raw)
	}
	mustCode(t, "other hospital's row", e.do(t, req{method: "GET", path: "/_test/clinical-queries/" + rowB.String(), bearer: atA}),
		404, "not_found")
	mustCode(t, "unknown row", e.do(t, req{method: "GET", path: "/_test/clinical-queries/" + uuid.NewString(), bearer: atA}),
		404, "not_found")
}
