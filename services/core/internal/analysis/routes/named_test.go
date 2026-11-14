package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

const synthDrill = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestNamedLimitsPinned(t *testing.T) {
	if NamedPerDay != 5 || ClearanceMePerMinute != 6 {
		t.Errorf("named %d a day, clearance/me %d a minute; want 5 and 6", NamedPerDay, ClearanceMePerMinute)
	}
	if !NamedRule.NeedsMFA || !NamedRule.Write || !NamedRule.NoIdempotency || NamedRule.Platform || strings.Join(NamedRule.AnyRole, ",") != "hospital_admin,analyst" {
		t.Errorf("NamedRule %+v", NamedRule)
	}
	for code, status := range map[string]int{"not_cleared": 403, "clearance_ended": 403, "idempotency_mismatch": 422, "expired": 409,
		"named_access_off": 403} {
		w := httptest.NewRecorder()
		httpx.Error(w, httptest.NewRequest(http.MethodGet, "/", nil), code)
		if got := w.Code; got != status {
			t.Errorf("%s: %d, want %d", code, got, status)
		}
	}
}

// The body: exactly its fields (T19: a hospital, viewer or clearance id
// is refused), a known purpose, a note of 1-200 characters with no
// control characters, required for other.
func TestParseNamedBody(t *testing.T) {
	good := `{"drill":"` + synthDrill + `","purpose":"audit","filters":{"sex":"female"},"period":{"level":"week","start":"2026-10-01"}}`
	for name, c := range map[string]struct {
		body string
		ok   bool
	}{
		"good":            {good, true},
		"other with note": {strings.Replace(good, `"audit"`, `"other","note":"Synthetic follow-up of a ward audit"`, 1), true},
		"other no note":   {strings.Replace(good, `"audit"`, `"other"`, 1), false},
		"empty note":      {strings.Replace(good, `"audit"`, `"audit","note":""`, 1), false},
		"long note":       {strings.Replace(good, `"audit"`, `"audit","note":"`+strings.Repeat("x", 201)+`"`, 1), false},
		"control in note": {strings.Replace(good, `"audit"`, `"audit","note":"a\u0007b"`, 1), false},
		"unknown purpose": {strings.Replace(good, `"audit"`, `"research"`, 1), false},
		"no period":       {`{"drill":"` + synthDrill + `","purpose":"audit"}`, false},
		"short drill":     {strings.Replace(good, synthDrill, "abc", 1), false},
		"period filter":   {strings.Replace(good, `{"sex":"female"}`, `{"from":"2026-10-01"}`, 1), false},
	} {
		var b namedBody
		err := json.Unmarshal([]byte(c.body), &b)
		if err == nil {
			_, err = parseNamed(b)
		}
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
	// T19: unknown fields are refused by the strict decoder the route uses.
	for _, f := range []string{"hospital_id", "viewer", "clearance_id", "clearance", "user_id"} {
		body := strings.Replace(good, `{"drill"`, `{"`+f+`":"00000000-0000-4000-8000-000000000001","drill"`, 1)
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		if httpx.Decode(httptest.NewRecorder(), r, &namedBody{}) {
			t.Errorf("%s accepted", f)
		}
	}
	// The canonical body binds the parsed values, not the bytes.
	var a, b namedBody
	_ = json.Unmarshal([]byte(good), &a)
	_ = json.Unmarshal([]byte(`{"period":{"start":"2026-10-01","level":"week"},"filters":{"sex":"female"},"purpose":"audit","drill":"`+synthDrill+`"}`), &b)
	if string(canonical(a, disclose.Filters{})) != string(canonical(b, disclose.Filters{})) {
		t.Error("the same request in another order binds differently")
	}
}

func TestIdemKeyShape(t *testing.T) {
	for k, ok := range map[string]bool{"synthetic-key-1": true, "": false, "has space": false, strings.Repeat("k", 256): false} {
		if idemKeyOK([]string{k}) != ok {
			t.Errorf("%q", k)
		}
	}
	if idemKeyOK([]string{"a", "b"}) || idemKeyOK(nil) {
		t.Error("not exactly one key accepted")
	}
}

// The named actions' payloads: ids and codes only.
func TestNamedActionsSchemas(t *testing.T) {
	good := map[string]any{"view": "affected", "period": "week:2026-10-01", "filters": map[string]any{"sex": "female"},
		"patient_count": 20, "rows": 20, "purpose": "audit", "replays": 0, "clearance_id": "00000000-0000-4000-8000-0000000000aa"}
	if err := audit.CheckActionPayload("analysis.named_viewed", good); err != nil {
		t.Fatalf("named_viewed: %v", err)
	}
	for k, v := range map[string]any{"name": "Zephyrine Quenby (synthetic)", "mrn": "SYN-1", "note": "x", "pid": "P-ABCDEFGH"} {
		bad := map[string]any{k: v}
		for kk, vv := range good {
			bad[kk] = vv
		}
		if audit.CheckActionPayload("analysis.named_viewed", bad) == nil {
			t.Errorf("named_viewed took %s", k)
		}
	}
	if err := audit.CheckActionPayload("analysis.named_view_failed", map[string]any{"clearance_id": "00000000-0000-4000-8000-0000000000aa"}); err != nil {
		t.Error(err)
	}
	if err := audit.CheckActionPayload("analysis.named_limit_reached", map[string]any{"limit": 5}); err != nil {
		t.Error(err)
	}
}
