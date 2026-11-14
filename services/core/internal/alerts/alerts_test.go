package alerts_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/alerts"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// A16, design §12.2: routing never goes to nobody. With every named
// clinician gone (the caller passes the active ones only) and the queue
// off, the alert goes to the queue.
func TestAllNamedDeactivatedQueueOffFallsBackToQueue(t *testing.T) {
	queue, users, reason := alerts.Route(alerts.Settings{Queue: false}, nil, "critical", "blood_chemistry")
	if !queue || len(users) != 0 || reason != "" {
		t.Fatalf("queue %v users %v reason %q", queue, users, reason)
	}
	u := uuid.New()
	queue, users, _ = alerts.Route(alerts.Settings{Queue: false, Recipients: []uuid.UUID{u, u}}, nil, "critical", "blood_chemistry")
	if queue || !slices.Equal(users, []uuid.UUID{u}) {
		t.Fatalf("named only: queue %v users %v", queue, users)
	}
	queue, users, _ = alerts.Route(alerts.Settings{Queue: true, Recipients: []uuid.UUID{u}}, nil, "urgent", "blood_chemistry")
	if !queue || len(users) != 1 {
		t.Fatalf("both: queue %v users %v", queue, users)
	}
}

// Design §12.1 step 3, §12.2: a unit uncovered filters; an unknown or new
// unit, or no unit recorded, is covered; the category's level (default
// critical_urgent, critical for vital signs) admits the severity.
func TestRouteUnitsAndLevels(t *testing.T) {
	off, on := uuid.New(), uuid.New()
	s := alerts.Settings{Queue: true, Covered: map[uuid.UUID]bool{off: false, on: true},
		Levels: map[string]string{"blood_count": "off", "blood_sugar": "all"}}
	cases := []struct {
		unit               *uuid.UUID
		severity, category string
		reason             string
	}{
		{&off, "critical", "blood_chemistry", alerts.ReasonUnitNotCovered},
		{&on, "critical", "blood_chemistry", ""},
		{nil, "critical", "blood_chemistry", ""},
		{ptr(uuid.New()), "urgent", "blood_chemistry", ""},
		{nil, "info", "blood_chemistry", alerts.ReasonLevelOff},
		{nil, "urgent", "vital_signs", alerts.ReasonLevelOff},
		{nil, "critical", "vital_signs", ""},
		{nil, "critical", "blood_count", alerts.ReasonLevelOff},
		{nil, "info", "blood_sugar", ""},
	}
	for i, c := range cases {
		if _, _, reason := alerts.Route(s, c.unit, c.severity, c.category); reason != c.reason {
			t.Errorf("case %d: reason %q, want %q", i, reason, c.reason)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// The categories are the closed eight, each with a default level.
func TestCategoriesClosed(t *testing.T) {
	want := []string{"blood_chemistry", "blood_count", "kidney_function", "blood_gases_lactate", "heart_markers",
		"blood_sugar", "vital_signs", "infection_markers"}
	var got []string
	for _, c := range alerts.Categories {
		got = append(got, c.ID)
		if !slices.Contains(alerts.Levels, alerts.DefaultLevel(c.ID)) || c.Label == "" {
			t.Errorf("%s: default %q label %q", c.ID, alerts.DefaultLevel(c.ID), c.Label)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("categories %v", got)
	}
}

// Design §13.1: the alert routes are clinicians'; the settings, matches and
// exclusions hospital admins', with a two-step session; the match and
// exclusion writes ask for a step-up code; every write is a Write.
func TestAlertRouteRules(t *testing.T) {
	m := authz.NewMux(nil)
	alerts.Register(m, alerts.Deps{})
	rules := map[string]authz.Rule{}
	for _, rr := range m.Table() {
		rules[rr.Method+" "+strings.TrimPrefix(rr.Pattern, httpx.BasePath)] = rr.Rule
	}
	clin := []string{authz.RoleClinician}
	admin := []string{authz.RoleHospitalAdmin}
	for k, want := range map[string]authz.Rule{
		"GET /alerts":                                 {AnyRole: clin},
		"GET /alerts/unread-count":                    {AnyRole: clin},
		"POST /alerts/{id}/acknowledge":               {AnyRole: clin, Write: true},
		"POST /alerts/{id}/dismiss":                   {AnyRole: clin, Write: true},
		"GET /me/notification-settings":               {AnyRole: clin},
		"PATCH /me/notification-settings":             {AnyRole: clin, Write: true},
		"GET /hospital/alert-settings":                {AnyRole: admin, NeedsMFA: true},
		"PATCH /hospital/alert-settings":              {AnyRole: admin, NeedsMFA: true, Write: true},
		"GET /hospital/patient-matches":               {AnyRole: admin, NeedsMFA: true},
		"POST /hospital/patient-matches/{id}/confirm": {AnyRole: admin, NeedsMFA: true, Write: true, StepUp: true},
		"POST /hospital/patient-matches/{id}/reject":  {AnyRole: admin, NeedsMFA: true, Write: true, StepUp: true},
		"POST /hospital/patient-exclusions":           {AnyRole: admin, NeedsMFA: true, Write: true, StepUp: true},
	} {
		got, ok := rules[k]
		if !ok {
			t.Errorf("%s not registered", k)
			continue
		}
		if !slices.Equal(got.AnyRole, want.AnyRole) || got.NeedsMFA != want.NeedsMFA || got.Write != want.Write ||
			got.StepUp != want.StepUp || got.Platform || got.Public {
			t.Errorf("%s: %+v, want %+v", k, got, want)
		}
	}
	if len(rules) != 12 {
		t.Errorf("%d routes, want 12", len(rules))
	}
}

// A27 (route part): only clinicians reach the alert routes; a hospital
// admin who isn't a clinician, and a platform account, are refused by the
// guard before any handler runs (nil guard: every route 503; the table is
// what the guard enforces, pinned above). Here the handlers refuse a
// request with no principal at all.
func TestHandlersNeedAPrincipal(t *testing.T) {
	for k, h := range alerts.Handlers(alerts.Deps{}) {
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
