//go:build integration

package auth_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// User.features (analysis design §6.5): analysis is true only when the
// hospital's effective switch is on, the user is a hospital admin or an
// analyst, and two-step is on. named_access is analysis and the effective
// named-access position not off (analysis plan Task 22).

func setAnalysisSwitch(t *testing.T, hospital *uuid.UUID, position, ref string) {
	t.Helper()
	ctx := context.Background()
	if _, err := super.Exec(ctx, `DELETE FROM analysis_switch WHERE hospital_id IS NOT DISTINCT FROM $1`, hospital); err != nil {
		t.Fatal(err)
	}
	if _, err := super.Exec(ctx, `INSERT INTO analysis_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, $2, $3, $3, $3)`, hospital, position, ref); err != nil {
		t.Fatal(err)
	}
}

func features(t *testing.T, r resp) map[string]any {
	t.Helper()
	u, _ := r.body["user"].(map[string]any)
	f, ok := u["features"].(map[string]any)
	if !ok || len(f) != 2 {
		t.Fatalf("features %v (status %d)", u["features"], r.code)
	}
	if f["analysis"] == false && f["named_access"] != false {
		t.Fatalf("named_access %v without analysis", f["named_access"])
	}
	return f
}

func TestFeaturesOffByDefault(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	a := seedUser(t, hid, false, "hospital_admin")
	if f := features(t, e.fullSession(t, a, e.enrol(t, a))); f["analysis"] != false {
		t.Fatalf("no switch rows: analysis %v", f["analysis"])
	}
}

func TestFeaturesNeedRoleAndMFA(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	setAnalysisSwitch(t, nil, "on", "SO-2026-0001")
	setAnalysisSwitch(t, &hid, "on", "SO-2026-0002")

	c := seedUser(t, hid, false, "clinician")
	r := e.login(t, c.email, testPassword, newIP())
	if r.code != 200 {
		t.Fatalf("clinician login %d %s", r.code, r.raw)
	}
	if f := features(t, r); f["analysis"] != false {
		t.Error("a clinician without analyst: analysis true")
	}
	a := seedUser(t, hid, false, "hospital_admin")
	if f := features(t, e.limitedSession(t, a)); f["analysis"] != false {
		t.Error("an admin without two-step: analysis true")
	}
	if f := features(t, e.fullSession(t, a, e.enrol(t, a))); f["analysis"] != true {
		t.Error("an admin with two-step at a switched-on hospital: analysis false")
	}
	an := seedUser(t, hid, false, "clinician", "analyst")
	en := e.enrol(t, an)
	if f := features(t, e.fullSession(t, an, en)); f["analysis"] != true {
		t.Error("an analyst with two-step: analysis false")
	}
	// The global lowered: off at once, for everyone.
	setAnalysisSwitch(t, nil, "off", "SO-2026-0001")
	if f := features(t, e.fullSession(t, an, en)); f["analysis"] != false {
		t.Error("global off: analysis true")
	}
	setAnalysisSwitch(t, nil, "on", "SO-2026-0001")
	// Another hospital's switch gives nothing.
	other := seedHospital(t, hospitalOpts{})
	o := seedUser(t, other, false, "hospital_admin")
	if f := features(t, e.fullSession(t, o, e.enrol(t, o))); f["analysis"] != false {
		t.Error("a hospital with no row of its own: analysis true")
	}
}

func setNamedSwitch(t *testing.T, hospital *uuid.UUID, position string) {
	t.Helper()
	ctx := context.Background()
	if _, err := super.Exec(ctx, `DELETE FROM named_access_switch WHERE hospital_id IS NOT DISTINCT FROM $1`, hospital); err != nil {
		t.Fatal(err)
	}
	ref := any(nil)
	if position != "off" {
		ref = "SO-2026-0007"
	}
	if _, err := super.Exec(ctx, `INSERT INTO named_access_switch (hospital_id, position, signoff_clinical, signoff_privacy, signoff_legal)
		VALUES ($1, $2, $3, $3, $3)`, hospital, position, ref); err != nil {
		t.Fatal(err)
	}
}

// named_access: off with no rows; on at letters_only or on, for a person
// with analysis; off again the moment either row is lowered, or analysis.
func TestFeaturesNamedAccess(t *testing.T) {
	e := newEnv(t)
	hid := seedHospital(t, hospitalOpts{})
	setAnalysisSwitch(t, nil, "on", "SO-2026-0001")
	setAnalysisSwitch(t, &hid, "on", "SO-2026-0002")
	a := seedUser(t, hid, false, "hospital_admin")
	en := e.enrol(t, a)
	if f := features(t, e.fullSession(t, a, en)); f["analysis"] != true || f["named_access"] != false {
		t.Fatalf("no named rows: %v", f)
	}
	setNamedSwitch(t, nil, "on")
	setNamedSwitch(t, &hid, "letters_only")
	if f := features(t, e.fullSession(t, a, en)); f["named_access"] != true {
		t.Errorf("letters_only: %v", f)
	}
	c := seedUser(t, hid, false, "clinician")
	if r := e.login(t, c.email, testPassword, newIP()); r.code != 200 {
		t.Fatalf("clinician login %d", r.code)
	} else if f := features(t, r); f["named_access"] != false {
		t.Errorf("a clinician: %v", f)
	}
	setNamedSwitch(t, nil, "off")
	if f := features(t, e.fullSession(t, a, en)); f["named_access"] != false {
		t.Errorf("global lowered: %v", f)
	}
	setNamedSwitch(t, nil, "on")
	setAnalysisSwitch(t, &hid, "off", "SO-2026-0002")
	if f := features(t, e.fullSession(t, a, en)); f["analysis"] != false || f["named_access"] != false {
		t.Errorf("analysis off: %v", f)
	}
	setNamedSwitch(t, &hid, "off")
	setNamedSwitch(t, nil, "off")
}
