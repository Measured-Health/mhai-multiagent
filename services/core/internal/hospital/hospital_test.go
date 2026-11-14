package hospital_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/hospital"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Every route is a hospital admin's with a two-step session; every staff
// write and the settings PATCH are step-up routes; the routes core has no
// feature behind are writes like the rest and answer 501.
func TestHospitalRouteRules(t *testing.T) {
	m := authz.NewMux(nil)
	hospital.Register(m, hospital.Deps{})
	rules := map[string]authz.Rule{}
	for _, rr := range m.Table() {
		rules[rr.Method+" "+strings.TrimPrefix(rr.Pattern, httpx.BasePath)] = rr.Rule
	}
	admin := []string{authz.RoleHospitalAdmin}
	read := authz.Rule{AnyRole: admin, NeedsMFA: true}
	write := authz.Rule{AnyRole: admin, NeedsMFA: true, Write: true}
	stepUp := authz.Rule{AnyRole: admin, NeedsMFA: true, Write: true, StepUp: true}
	want := map[string]authz.Rule{
		"GET /hospital/staff":                               read,
		"PATCH /hospital/staff/{id}":                        stepUp,
		"POST /hospital/staff/{id}/deactivate":              stepUp,
		"POST /hospital/staff/{id}/reactivate":              stepUp,
		"GET /hospital/settings":                            read,
		"PATCH /hospital/settings":                          stepUp,
		"GET /hospital/usage":                               read,
		"GET /hospital/activity":                            read,
		"GET /hospital/review-oversight":                    read,
		"GET /hospital/corrections/{id}/eligible-reviewers": read,
		"POST /hospital/staff/invites":                      write,
		"DELETE /hospital/staff/{id}":                       write,
		"POST /hospital/staff/{id}/password-reset":          write,
		"POST /hospital/staff/{id}/reset-mfa":               write,
		"POST /hospital/staff/{id}/remind":                  write,
		"POST /hospital/staff/{id}/resend-invite":           write,
		"POST /hospital/corrections/{id}/remind":            write,
		"POST /hospital/corrections/{id}/assign":            write,
	}
	for k, w := range want {
		got, ok := rules[k]
		if !ok {
			t.Errorf("%s not registered", k)
			continue
		}
		if !slices.Equal(got.AnyRole, w.AnyRole) || got.NeedsMFA != w.NeedsMFA || got.Write != w.Write ||
			got.StepUp != w.StepUp || got.Platform || got.Public {
			t.Errorf("%s: %+v, want %+v", k, got, w)
		}
	}
	if len(rules) != len(want) {
		t.Errorf("%d routes, want %d", len(rules), len(want))
	}
}

// The handlers refuse a request with no principal (the guard always sets
// one); none answers anything else first.
func TestHandlersNeedAPrincipal(t *testing.T) {
	for k, h := range hospital.Handlers(hospital.Deps{}) {
		method, path, _ := strings.Cut(k, " ")
		path = strings.ReplaceAll(path, "{id}", uuid.NewString())
		r := httptest.NewRequest(method, httpx.BasePath+path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s with no principal: %d", k, w.Code)
		}
	}
}

func TestValidRoles(t *testing.T) {
	for _, c := range []struct {
		roles []string
		want  error
	}{
		{[]string{"clinician"}, nil},
		{[]string{"clinician", "analyst", "hospital_admin"}, nil},
		{[]string{"hospital_admin"}, nil},
		{nil, hospital.ErrInvalid},
		{[]string{"clinician", "clinician"}, hospital.ErrInvalid},
		{[]string{"platform_admin"}, hospital.ErrInvalid},
		{[]string{"auditor", "clinician"}, hospital.ErrInvalid},
		{[]string{"analyst"}, hospital.ErrEligible},
		{[]string{"analyst", "hospital_admin"}, hospital.ErrEligible},
	} {
		if err := hospital.ValidRoles(c.roles); !errors.Is(err, c.want) {
			t.Errorf("%v: %v, want %v", c.roles, err, c.want)
		}
	}
}

// Frontend requirements §5.1, §6 rule 7: an admin may add roles to
// themselves (flagged), never drop their own hospital_admin nor give
// themselves Analyst; the last active admin keeps the role; a stale
// version is a conflict.
func TestPlanRefusals(t *testing.T) {
	admin := []string{"clinician", "hospital_admin"}
	me := hospital.Person([]string{"hospital_admin"}, 3, true, true)
	other := hospital.Person(admin, 7, false, true)

	if _, _, _, err := hospital.Plan(2, []string{"hospital_admin", "clinician"}, nil, me, 1); !errors.Is(err, hospital.ErrConflict) {
		t.Errorf("stale version: %v", err)
	}
	if _, _, _, err := hospital.Plan(3, []string{"clinician"}, nil, me, 1); !errors.Is(err, hospital.ErrSelf) {
		t.Errorf("dropping own admin: %v", err)
	}
	if _, _, _, err := hospital.Plan(3, []string{"hospital_admin", "clinician", "analyst"}, nil, me, 1); !errors.Is(err, hospital.ErrSelf) {
		t.Errorf("own analyst: %v", err)
	}
	add, remove, self, err := hospital.Plan(3, []string{"hospital_admin", "clinician"}, nil, me, 1)
	if err != nil || !slices.Equal(add, []string{"clinician"}) || len(remove) != 0 || !self {
		t.Errorf("own clinician: %v %v %v %v", add, remove, self, err)
	}
	if _, _, _, err := hospital.Plan(7, []string{"clinician"}, nil, other, 0); !errors.Is(err, hospital.ErrLastAdmin) {
		t.Errorf("last admin: %v", err)
	}
	add, remove, self, err = hospital.Plan(7, []string{"clinician"}, nil, other, 1)
	if err != nil || len(add) != 0 || !slices.Equal(remove, []string{"hospital_admin"}) || self {
		t.Errorf("demote another: %v %v %v %v", add, remove, self, err)
	}
	// A deactivated admin can be demoted with no other admin check.
	gone := hospital.Person(admin, 1, false, false)
	if _, _, _, err := hospital.Plan(1, []string{"clinician"}, nil, gone, 0); err != nil {
		t.Errorf("demote a deactivated admin: %v", err)
	}
}

// A settings edit loosens sign-in when it turns clinicians' two-step off,
// trusted devices on, or lengthens the idle timeout; it reports field
// names only.
func TestSettingsLoosening(t *testing.T) {
	cur := hospital.SettingsOut{ReviewOverdueHours: 4, IdleTimeoutMinutes: 15, RequireMFAForClinicians: true}
	i := func(n int) *int { return &n }
	b := func(v bool) *bool { return &v }
	for _, c := range []struct {
		review, idle *int
		mfa, trust   *bool
		fields       []string
		loosened     bool
	}{
		{review: i(8), fields: []string{"review_overdue_hours"}},
		{idle: i(10), fields: []string{"idle_timeout_minutes"}},
		{idle: i(30), fields: []string{"idle_timeout_minutes"}, loosened: true},
		{mfa: b(false), fields: []string{"require_mfa_for_clinicians"}, loosened: true},
		{mfa: b(true), fields: []string{}},
		{trust: b(true), fields: []string{"allow_trust_device"}, loosened: true},
		{review: i(4), idle: i(15), fields: []string{}},
	} {
		f, l := hospital.SettingsChange(c.review, c.idle, c.mfa, c.trust, cur)
		if !slices.Equal(f, c.fields) || l != c.loosened {
			t.Errorf("%+v: %v %v", c, f, l)
		}
	}
}

// Analysis Task 17: the six refusals of migration 00030 and the staff
// rules map to their codes.
func TestCodes(t *testing.T) {
	for err, want := range map[error]string{
		analysis.ErrAnalystForbidden:              "forbidden",
		analysis.ErrAnalystNotFound:               "not_found",
		analysis.ErrAnalystSelf:                   "self_change",
		analysis.ErrAnalystOff:                    "analysis_off",
		analysis.ErrAnalystCap:                    "analyst_cap",
		analysis.ErrAnalystNotEligible:            "not_eligible",
		hospital.ErrSelf:                          "self_change",
		hospital.ErrLastAdmin:                     "last_admin",
		hospital.ErrConflict:                      "conflict",
		hospital.ErrInvalid:                       "validation_failed",
		hospital.ErrEligible:                      "not_eligible",
		errors.New("driver: SYNTHETIC-LEAK-a91c"): "internal_error",
	} {
		if got := hospital.CodeFor(err); got != want {
			t.Errorf("%v: %q, want %q", err, got, want)
		}
		if _, ok := httpx.Codes[want]; !ok {
			t.Errorf("%q not in httpx.Codes", want)
		}
	}
}

func TestActivityQuery(t *testing.T) {
	good := []string{"", "type=admin", "person=Ada", "staff_id=" + uuid.NewString(), "from=2026-10-01&to=2026-11-01",
		"from=2026-10-01T00:00:00Z", "flagged=true", "cursor=42", "limit=100"}
	for _, q := range good {
		if _, err := hospital.ParseActivityQuery(q); err != nil {
			t.Errorf("%q refused: %v", q, err)
		}
	}
	bad := []string{"type=unknown", "type=admin&type=review", "person=", "person=" + strings.Repeat("x", 65),
		"staff_id=nope", "staff_id=" + strings.ToUpper(uuid.NewString()), "from=yesterday", "from=2026-11-01&to=2026-10-01",
		"flagged=yes", "cursor=0", "cursor=-1", "cursor=01", "limit=0", "limit=101", "other=1", "%zz"}
	for _, q := range bad {
		if _, err := hospital.ParseActivityQuery(q); !errors.Is(err, hospital.ErrInvalid) {
			t.Errorf("%q: %v, want invalid", q, err)
		}
	}
}

// Every activity kind is one of the contract's, under a contract type.
func TestActivityKindsInContract(t *testing.T) {
	kinds := []string{"signed_in", "signed_in_without_mfa", "sign_in_locked", "consultation_run", "result_agreed",
		"correction_suggested", "correction_approved", "correction_rejected", "correction_withdrawn", "roles_changed",
		"specialty_changed", "deactivated", "reactivated", "settings_changed", "analyst_granted", "analyst_removed",
		"alert_units_changed", "alert_rules_changed", "alert_recipients_changed", "self_role_change"}
	for a, k := range hospital.ActivityActions {
		if !slices.Contains(kinds, k.Kind()) || !slices.Contains(hospital.ActivityTypes, k.Type()) {
			t.Errorf("%s: %s/%s not in the contract", a, k.Type(), k.Kind())
		}
	}
}

func TestPeriodBounds(t *testing.T) {
	lagos, err := time.LoadLocation("Africa/Lagos")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 11, 14, 10, 30, 0, 0, time.UTC)
	from, to, ok := hospital.PeriodBounds("last_month", now, lagos)
	if !ok || !from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, lagos)) || !to.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, lagos)) {
		t.Errorf("last_month %v %v", from, to)
	}
	from, to, ok = hospital.PeriodBounds("this_month", now, lagos)
	if !ok || !from.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, lagos)) || !to.Equal(now) {
		t.Errorf("this_month %v %v", from, to)
	}
	from, _, ok = hospital.PeriodBounds("last_90_days", now, lagos)
	if !ok || !from.Equal(time.Date(2026, 8, 17, 0, 0, 0, 0, lagos)) {
		t.Errorf("last_90_days %v", from)
	}
	if _, _, ok := hospital.PeriodBounds("this_year", now, lagos); ok {
		t.Error("an unknown period")
	}
	if hospital.Ref(uuid.MustParse("abcdef12-0000-4000-8000-000000000000")) != "C-ABCDEF" {
		t.Error("ref")
	}
}
