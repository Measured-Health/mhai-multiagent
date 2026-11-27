package audit_test

import (
	"encoding/json"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// TestActionsMapToKnownWebKinds: the actions once sent bare that mhai-web
// already has a kind for (27 Nov) map to that kind, with its type, the
// target it takes (or none) and only the facts listed here; each mapped
// event is held against the vendored vocabulary as mhai-web reads it.
func TestActionsMapToKnownWebKinds(t *testing.T) {
	voc := loadVocabulary(t)
	const (
		user   = "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11"
		hosp   = "0b6c6d4e-8a9f-4f6e-b2d3-1c2a3b4c5d6e"
		hold   = "1a2b3c4d-5e6f-4a1b-8c2d-3e4f5a6b7c8d"
		change = "9f8e7d6c-5b4a-4f3e-9d2c-1b0a9f8e7d6c"
	)
	staff := &audit.Target{Kind: "staff", ID: user}
	team := &audit.Target{Kind: "team_member", ID: user}
	held := &audit.Target{Kind: "hold", ID: hold}
	for _, c := range []struct {
		name      string
		ev        audit.Event
		typ, kind string
		target    *audit.Target
		facts     map[string]any
	}{
		{"staff invited (hospital admin)", audit.Event{Action: "staff.invited", Entity: "user", EntityID: user, HospitalID: hosp,
			Payload: map[string]any{"roles": []any{"clinician"}}}, "staff", "staff_invited", staff, nil},
		{"staff invited (first admin)", audit.Event{Action: "staff.invited", Entity: "user", EntityID: user, HospitalID: hosp,
			Payload: map[string]any{"roles": []any{"hospital_admin"}}}, "staff", "staff_invited", staff, nil},
		{"staff roles changed", audit.Event{Action: "staff.roles_changed", Entity: "user", EntityID: user, HospitalID: hosp,
			Payload: map[string]any{"added": []any{"hospital_admin"}, "removed": []any{}, "self": false}}, "staff", "staff_role_changed", staff, nil},
		{"staff deactivated", audit.Event{Action: "staff.deactivated", Entity: "user", EntityID: user, HospitalID: hosp,
			Payload: map[string]any{"sessions_revoked": 2.0}}, "staff", "staff_deactivated", staff, nil},
		{"staff reactivated", audit.Event{Action: "staff.reactivated", Entity: "user", EntityID: user, HospitalID: hosp,
			Payload: map[string]any{}}, "staff", "staff_reactivated", staff, nil},
		{"analyst granted", audit.Event{Action: "analyst.granted", Entity: "user", EntityID: user, HospitalID: hosp,
			Payload: map[string]any{"change_id": change, "via": "app"}}, "staff", "staff_role_changed", staff, nil},
		{"analyst removed", audit.Event{Action: "analyst.removed", Entity: "user", EntityID: user, HospitalID: hosp,
			Payload: map[string]any{"change_id": change, "via": "cli", "reason": "clinician_removed"}}, "staff", "staff_role_changed", staff, nil},
		{"model policy changed", audit.Event{Action: "hospital.model_policy_changed", Entity: "hospital", EntityID: hosp,
			Payload: map[string]any{"hospital_id": hosp, "provider": "none", "previous": "none", "monthly_token_budget": 1000.0,
				"via": "platform", "own_model": true, "previous_own_model": false}}, "platform", "model_policy_changed", nil, nil},
		{"analysis switch raised", audit.Event{Action: "analysis.switch_raised",
			Payload: map[string]any{"hospital_id": hosp, "via": "cli", "refs": "demo"}}, "platform", "analysis_switched", nil,
			map[string]any{"switched": "on"}},
		{"analysis switch lowered (global)", audit.Event{Action: "analysis.switch_lowered",
			Payload: map[string]any{"via": "cli"}}, "platform", "analysis_switched", nil, map[string]any{"switched": "off"}},
		{"analysis viewed", audit.Event{Action: "analysis.viewed", HospitalID: hosp,
			Payload: map[string]any{"view": "prevalence", "period": "month:2026-10-01", "filters": map[string]any{"sex": "female"}}},
			"analysis", "analysis_viewed", nil, nil},
		{"team role changed", audit.Event{Action: "platform.team_role_changed", Entity: "user", EntityID: user,
			Payload: map[string]any{"user_id": user, "role": "auditor", "previous": "platform_admin"}}, "staff", "team_role_changed", team, nil},
		{"team member removed", audit.Event{Action: "platform.team_member_removed", Entity: "user", EntityID: user,
			Payload: map[string]any{"user_id": user, "role": "auditor"}}, "staff", "team_removed", team, nil},
		{"analytics email changed", audit.Event{Action: "platform.analytics_email_changed", Entity: "user", EntityID: user,
			Payload: map[string]any{"mode": "off", "previous": "critical"}}, "platform", "analytics_email_changed", nil, nil},
		{"hold placed (platform)", audit.Event{Action: "legal_hold.placed", Entity: "legal_hold", EntityID: hold,
			Payload: map[string]any{"hold_id": hold, "hospital_id": hosp, "reason": "court_case"}}, "retention", "hold_placed", held, nil},
		{"hold placed (cli)", audit.Event{Action: "legal_hold.placed", Entity: "legal_hold", EntityID: hold, HospitalID: hosp,
			Payload: map[string]any{"hold_id": hold, "via": "cli"}}, "retention", "hold_placed", held, nil},
		{"hold lifted (platform)", audit.Event{Action: "legal_hold.lifted", Entity: "legal_hold", EntityID: hold,
			Payload: map[string]any{"hold_id": hold, "hospital_id": hosp, "lift_id": change}}, "retention", "hold_lifted", held, nil},
		{"hold lifted (cli)", audit.Event{Action: "legal_hold.lifted", Entity: "legal_hold", EntityID: hold, HospitalID: hosp,
			Payload: map[string]any{"hold_id": hold, "via": "cli"}}, "retention", "hold_lifted", held, nil},
		{"hold lift requested", audit.Event{Action: "legal_hold.lift_requested", Entity: "legal_hold", EntityID: hold,
			Payload: map[string]any{"hold_id": hold, "hospital_id": hosp, "lift_id": change, "reason": "case_closed"}},
			"retention", "hold_lift_requested", held, nil},
		{"hold lift cancelled", audit.Event{Action: "legal_hold.lift_cancelled", Entity: "legal_hold", EntityID: hold,
			Payload: map[string]any{"hold_id": hold, "hospital_id": hosp, "lift_id": change}}, "retention", "hold_lift_cancelled", held, nil},
		{"hospital settings changed", audit.Event{Action: "hospital_settings.changed", HospitalID: hosp,
			Payload: map[string]any{"fields": []any{"idle_timeout_minutes"}, "loosened": true}}, "hospital_settings", "setting_changed", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := audit.CheckActionPayload(c.ev.Action, c.ev.Payload); err != nil {
				t.Fatalf("the test's payload doesn't fit %s's schema: %v", c.ev.Action, err)
			}
			m, ok := audit.Map(c.ev)
			if !ok {
				t.Fatalf("%s is sent bare; want %s/%s", c.ev.Action, c.typ, c.kind)
			}
			if m.Type != c.typ || m.Kind != c.kind || m.Outcome != "succeeded" || m.Flag != nil {
				t.Errorf("%s mapped to %s/%s outcome %s flag %v; want %s/%s, succeeded, no flag",
					c.ev.Action, m.Type, m.Kind, m.Outcome, m.Flag, c.typ, c.kind)
			}
			if (m.Target == nil) != (c.target == nil) || (m.Target != nil && *m.Target != *c.target) {
				t.Errorf("%s target %+v, want %+v", c.ev.Action, m.Target, c.target)
			}
			want := c.facts
			if want == nil {
				want = map[string]any{}
			}
			if got, exp := mustJSON(t, m.Facts), mustJSON(t, want); got != exp {
				t.Errorf("%s facts %s, want %s", c.ev.Action, got, exp)
			}
			voc.checkMapped(t, c.ev.Action, m)
			// The payload's own values (roles, providers, reasons, ids)
			// never reach the entry, whatever the facts.
			raw, _ := json.Marshal(m)
			for _, k := range []string{"roles", "provider", "previous", "mode", "reason", "hold_id", "hospital_id", "change_id", "fields", "period"} {
				if _, leaked := m.Facts[k]; leaked {
					t.Errorf("%s sends facts.%s: %s", c.ev.Action, k, raw)
				}
			}
		})
	}
}

// TestKnownKindsNeedTheirEntity: a staff, team or hold row whose entity
// isn't the one expected maps with no target rather than a wrong one.
func TestKnownKindsNeedTheirEntity(t *testing.T) {
	m, ok := audit.Map(audit.Event{Action: "staff.deactivated", Entity: "hospital", EntityID: "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11",
		Payload: map[string]any{"sessions_revoked": 0.0}})
	if !ok || m.Target != nil {
		t.Errorf("staff.deactivated on a hospital entity: %+v %v", m, ok)
	}
	m, ok = audit.Map(audit.Event{Action: "legal_hold.placed", Entity: "user", EntityID: "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11",
		Payload: map[string]any{"hold_id": "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11", "via": "cli"}})
	if !ok || m.Target != nil {
		t.Errorf("legal_hold.placed on a user entity: %+v %v", m, ok)
	}
}
