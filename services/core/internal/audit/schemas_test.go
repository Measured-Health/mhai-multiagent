package audit_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
)

// actionArgs are the helpers that take an audit action as a plain argument,
// and the argument's index.
var actionArgs = map[string]int{
	"writeAudit":     3, // auth: (ctx, tx, r, action, ...)
	"auditOnly":      2, // auth: (w, r, action, ...)
	"revokeSessions": 4, // auth: (ctx, r, user, hospital, action, ...)
	"jobEvent":       1, // intel: (row, action, payload)
}

// collectActions walks every non-test Go file under internal/ and cmd/ and
// returns each audit action written as a literal: an `Action: "..."` field,
// a literal passed to one of actionArgs, or a literal assigned to a
// variable named action or a field named Action.
func collectActions(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	found := map[string]string{}
	lit := func(e ast.Expr) (string, bool) {
		b, ok := e.(*ast.BasicLit)
		if !ok || b.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(b.Value)
		return s, err == nil
	}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			add := func(e ast.Expr) {
				if s, ok := lit(e); ok {
					found[s] = fset.Position(e.Pos()).String()
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.KeyValueExpr:
					if k, ok := n.Key.(*ast.Ident); ok && k.Name == "Action" {
						add(n.Value)
					}
				case *ast.CallExpr:
					name := ""
					switch fn := n.Fun.(type) {
					case *ast.Ident:
						name = fn.Name
					case *ast.SelectorExpr:
						name = fn.Sel.Name
					}
					if i, ok := actionArgs[name]; ok && i < len(n.Args) {
						add(n.Args[i])
					}
				case *ast.AssignStmt:
					for i, l := range n.Lhs {
						name := ""
						switch l := l.(type) {
						case *ast.Ident:
							name = l.Name
						case *ast.SelectorExpr:
							name = l.Sel.Name
						}
						if (name == "action" || name == "Action") && i < len(n.Rhs) && len(n.Lhs) == len(n.Rhs) {
							add(n.Rhs[i])
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return found
}

// TestEveryActionHasSchema (C21, core part): every audit action in the code
// has a payload schema in the registry.
func TestEveryActionHasSchema(t *testing.T) {
	found := collectActions(t)
	// The walk must see each kind of call site, or it proves nothing.
	for _, want := range []string{
		"query.submitted",             // Action: field
		"auth.login.succeeded",        // writeAudit
		"auth.login.failed",           // auditOnly
		"auth.logout",                 // revokeSessions
		"job.retried",                 // jobEvent
		"auth.mfa.backup_used",        // action := / action, payload =
		"feedback.validated",          // Plan 4
		"correction.submitted",        // Plan 4
		"correction.approved",         // Plan 4
		"correction.rejected",         // Plan 4
		"correction.withdrawn",        // Plan 4
		"correction.decision_refused", // Plan 4
	} {
		if _, ok := found[want]; !ok {
			t.Errorf("the walk didn't find %q", want)
		}
	}
	var missing []string
	for a, at := range found {
		if !audit.Registered(a) {
			missing = append(missing, a+" ("+at+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("no audit schema for %s", m)
	}
}

// TestAuditWriteRefusesBadPayload: an action with no schema, a missing,
// extra or ill-shaped key is refused before the chain is touched (the nil
// transaction is never used), with an error that never echoes a value.
func TestAuditWriteRefusesBadPayload(t *testing.T) {
	const marker = "SYNTHETIC-AUDIT-MARKER-91c2"
	cid, qid, jid := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for name, e := range map[string]audit.Event{
		"no schema":           {Action: "made.up", Payload: map[string]any{}},
		"missing required":    {Action: "correction.approved", Payload: map[string]any{"correction_id": cid}},
		"extra key":           {Action: "correction.withdrawn", Payload: map[string]any{"correction_id": cid, "note": marker}},
		"free text in an id":  {Action: "feedback.validated", Payload: map[string]any{"query_id": marker}},
		"upper-case uuid":     {Action: "feedback.validated", Payload: map[string]any{"query_id": strings.ToUpper(qid)}},
		"number for an id":    {Action: "feedback.validated", Payload: map[string]any{"query_id": 5}},
		"unknown code":        {Action: "correction.rejected", Payload: map[string]any{"correction_id": cid, "reason_code": marker}},
		"refusal code":        {Action: "correction.decision_refused", Payload: map[string]any{"correction_id": cid, "reason": "conflict", "attempted": "approve"}},
		"refusal attempt":     {Action: "correction.decision_refused", Payload: map[string]any{"correction_id": cid, "reason": "forbidden", "attempted": "delete"}},
		"nested object":       {Action: "correction.submitted", Payload: map[string]any{"correction_id": cid, "query_id": map[string]any{"x": qid}}},
		"diagnosis smuggled":  {Action: "correction.submitted", Payload: map[string]any{"correction_id": cid, "query_id": qid, "corrected_diagnosis": marker}},
		"job id not an id":    {Action: "correction.approved", Payload: map[string]any{"correction_id": cid, "job_id": "job-1"}},
		"frozen plan 2 shape": {Action: "auth.login.failed", Payload: map[string]any{"reason": marker}},
		"fraction count":      {Action: "auth.password.changed", Payload: map[string]any{"sessions_revoked": 1.5}},
	} {
		err := audit.Write(context.Background(), nil, e)
		if !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: err %v, want ErrSchema", name, err)
		}
		if err != nil && strings.Contains(err.Error(), marker) {
			t.Errorf("%s: the error echoes a value: %v", name, err)
		}
	}
	// The same shapes fit when they are right.
	for name, e := range map[string]audit.Event{
		"validated": {Action: "feedback.validated", Payload: map[string]any{"query_id": qid}},
		"submitted": {Action: "correction.submitted", Payload: map[string]any{"correction_id": cid, "query_id": qid}},
		"approved":  {Action: "correction.approved", Payload: map[string]any{"correction_id": cid, "job_id": jid}},
		"rejected":  {Action: "correction.rejected", Payload: map[string]any{"correction_id": cid, "reason_code": "unsafe"}},
		"withdrawn": {Action: "correction.withdrawn", Payload: map[string]any{"correction_id": cid}},
		"refused":   {Action: "correction.decision_refused", Payload: map[string]any{"correction_id": cid, "reason": "self_approval", "attempted": "reject"}},
		"plan 2":    {Action: "auth.login.succeeded", Payload: map[string]any{"mfa": true, "amr": []string{"pwd", "otp"}}},
		"plan 3":    {Action: "query.submitted", Payload: map[string]any{"query_id": qid, "urgency": "urgent"}},
	} {
		if err := audit.CheckPayload(e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Register is closed at init: a second schema for an action, or a required
// key with no check, stops the binary.
func TestRegisterRefusesMistakes(t *testing.T) {
	for name, f := range map[string]func(){
		"duplicate":          func() { audit.Register("correction.approved", audit.Schema{}) },
		"required unchecked": func() { audit.Register("test.unchecked", audit.Schema{Required: []string{"x"}}) },
		"malformed name":     func() { audit.Register("Not An Action", audit.Schema{}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

// hospital.model_policy_changed (cloud model providers plan, Task 2): a
// closed payload of ids, closed codes and a bounded integer; the agreement
// reference has the id shape, never free text.
func TestAuditSchemaModelPolicyChanged(t *testing.T) {
	const action = "hospital.model_policy_changed"
	hid := uuid.NewString()
	good := func() map[string]any {
		return map[string]any{"hospital_id": hid, "provider": "anthropic", "previous": "none",
			"agreement_ref": "DEMO-SYNTHETIC", "monthly_token_budget": 2000000, "via": "platform"}
	}
	if err := audit.CheckActionPayload(action, good()); err != nil {
		t.Fatalf("good payload: %v", err)
	}
	for _, ref := range []string{"AGR-2026-0042", "DPA-2026-17"} {
		m := good()
		m["agreement_ref"] = ref
		if err := audit.CheckActionPayload(action, m); err != nil {
			t.Errorf("%s: %v", ref, err)
		}
	}
	gemini := good()
	gemini["provider"], gemini["previous"] = "gemini", "xai"
	if err := audit.CheckActionPayload(action, gemini); err != nil {
		t.Errorf("gemini: %v", err)
	}
	off := good()
	off["provider"], off["previous"], off["via"] = "none", "xai", "cli"
	delete(off, "agreement_ref")
	if err := audit.CheckActionPayload(action, off); err != nil {
		t.Fatalf("switched off, no reference: %v", err)
	}
	for name, change := range map[string]func(m map[string]any){
		"free text reference":  func(m map[string]any) { m["agreement_ref"] = "signed with Dr Synthetic on Monday" },
		"lower-case reference": func(m map[string]any) { m["agreement_ref"] = "demo-synthetic" },
		"short reference":      func(m map[string]any) { m["agreement_ref"] = "AB" },
		"old free shape":       func(m map[string]any) { m["agreement_ref"] = "SYNTH-1" },
		"patient pseudonym":    func(m map[string]any) { m["agreement_ref"] = "P-AB12CD34" },
		"a person's name":      func(m map[string]any) { m["agreement_ref"] = "ADAEZE-OKAFOR" },
		"an MRN shape":         func(m map[string]any) { m["agreement_ref"] = "HAV-000123" },
		"a name and a number":  func(m map[string]any) { m["agreement_ref"] = "OKAFOR-2026-01" },
		"reference while off":  func(m map[string]any) { m["provider"] = "none" },
		"unknown provider":     func(m map[string]any) { m["provider"] = "mistral" },
		"unknown previous":     func(m map[string]any) { m["previous"] = "local" },
		"missing previous":     func(m map[string]any) { delete(m, "previous") },
		"missing provider":     func(m map[string]any) { delete(m, "provider") },
		"missing hospital":     func(m map[string]any) { delete(m, "hospital_id") },
		"missing budget":       func(m map[string]any) { delete(m, "monthly_token_budget") },
		"missing via":          func(m map[string]any) { delete(m, "via") },
		"budget 0":             func(m map[string]any) { m["monthly_token_budget"] = 0 },
		"budget too big":       func(m map[string]any) { m["monthly_token_budget"] = 1000000001 },
		"budget fraction":      func(m map[string]any) { m["monthly_token_budget"] = 1.5 },
		"unknown via":          func(m map[string]any) { m["via"] = "api" },
		"hospital not a uuid":  func(m map[string]any) { m["hospital_id"] = "HAV" },
		"extra key":            func(m map[string]any) { m["note"] = "synthetic" },
		"null provider":        func(m map[string]any) { m["provider"] = nil },
	} {
		m := good()
		change(m)
		if err := audit.CheckActionPayload(action, m); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A21, the EMR connection actions (design §12.6): ids, the type, the keys
// that changed and closed codes; never a value, an address or a label.
func TestAuditSchemasEMRConnection(t *testing.T) {
	hid, cid := uuid.NewString(), uuid.NewString()
	base := func(kv ...any) map[string]any {
		m := map[string]any{"hospital_id": hid, "connection_id": cid}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	good := map[string]map[string]any{
		"emr.connection_added": base("type", "fhir_r4"),
		"emr.connection_changed": base("fields_changed", []any{"base_url", "private_key"},
			"credentials_replaced", true, "credentials_wiped", true),
		"emr.connection_switched_on":  base(),
		"emr.connection_switched_off": base(),
		"emr.connection_tested":       base("outcome", "sign_in_refused"),
		"emr.source_resumed":          base("reason", "platform_resume"),
	}
	for action, p := range good {
		if err := audit.CheckActionPayload(action, p); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		// An unknown key is refused (a value smuggled under any name).
		bad := maps.Clone(p)
		bad["api_key"] = "synthetic-test-secret-not-real"
		if err := audit.CheckActionPayload(action, bad); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s with an unknown key: %v", action, err)
		}
		missing := maps.Clone(p)
		delete(missing, "connection_id")
		if err := audit.CheckActionPayload(action, missing); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s without connection_id: %v", action, err)
		}
	}
	for name, p := range map[string]map[string]any{
		"unknown type":            base("type", "hl7v2_feed"),
		"a value in fields":       base("fields_changed", []any{"https://fhir.example.org"}, "credentials_replaced", false, "credentials_wiped", false),
		"wiped not a bool":        base("fields_changed", []any{}, "credentials_replaced", false, "credentials_wiped", "yes"),
		"free text outcome":       base("outcome", "the server said no"),
		"hospital id not an id":   {"hospital_id": "HAV", "connection_id": cid, "type": "stub"},
		"connection id not an id": {"hospital_id": hid, "connection_id": "Main record", "type": "stub"},
	} {
		action := "emr.connection_added"
		switch {
		case p["fields_changed"] != nil:
			action = "emr.connection_changed"
		case p["outcome"] != nil:
			action = "emr.connection_tested"
		}
		if err := audit.CheckActionPayload(action, p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The EMR patient index actions (plan Task 16): ids, closed codes and
// counts only; a name, an MRN or a free-text reason is refused.
func TestAuditSchemasEMRPatients(t *testing.T) {
	pid, mid := uuid.NewString(), uuid.NewString()
	good := map[string]map[string]any{
		"emr.patients_merged": {"match_id": mid, "merge_id": uuid.NewString(), "kept_patient_id": pid,
			"merged_patient_id": uuid.NewString()},
		"emr.patient_match_rejected": {"match_id": mid},
		"emr.patient_excluded": {"patient_id": pid, "reason": "objection", "via": "route", "observations": 3,
			"medications": 0, "encounters": 1, "raw_records": 5, "matches": 0},
	}
	for action, p := range good {
		if err := audit.CheckActionPayload(action, p); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		bad := maps.Clone(p)
		bad["mrn"] = "MRN-SYNTHETIC-1"
		if err := audit.CheckActionPayload(action, bad); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s with an MRN: %v", action, err)
		}
	}
	for name, p := range map[string]map[string]any{
		"free-text reason": {"patient_id": pid, "reason": "the patient asked", "via": "cli", "observations": 0,
			"medications": 0, "encounters": 0, "raw_records": 0, "matches": 0},
		"name as patient": {"patient_id": "Synthetic Patient", "reason": "other", "via": "cli", "observations": 0,
			"medications": 0, "encounters": 0, "raw_records": 0, "matches": 0},
		"no counts": {"patient_id": pid, "reason": "other", "via": "cli"},
	} {
		if err := audit.CheckActionPayload("emr.patient_excluded", p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The rule set switch (EMR plan Task 30): a version or shadow, the one it
// replaced, the file's hash in hex.
func TestAuditSchemaRuleSetSwitched(t *testing.T) {
	hid := uuid.NewString()
	sha := strings.Repeat("ab", 32)
	for _, p := range []map[string]any{
		{"hospital_id": hid, "version": "1", "previous": "shadow", "sha256": sha, "via": "platform"},
		{"hospital_id": hid, "version": "shadow", "previous": "1", "via": "platform"},
	} {
		if err := audit.CheckActionPayload("emr.rule_set_switched", p); err != nil {
			t.Errorf("%v: %v", p, err)
		}
	}
	for name, p := range map[string]map[string]any{
		"short hash":     {"hospital_id": hid, "version": "1", "previous": "shadow", "sha256": "abcd", "via": "platform"},
		"free text":      {"hospital_id": hid, "version": "Version One", "previous": "shadow", "sha256": sha, "via": "platform"},
		"via cli":        {"hospital_id": hid, "version": "1", "previous": "shadow", "sha256": sha, "via": "cli"},
		"unknown key":    {"hospital_id": hid, "version": "1", "previous": "shadow", "sha256": sha, "via": "platform", "note": "x"},
		"not a hospital": {"hospital_id": "x", "version": "1", "previous": "shadow", "sha256": sha, "via": "platform"},
	} {
		if err := audit.CheckActionPayload("emr.rule_set_switched", p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A21 (EMR plan Tasks 31-34): the alert actions carry ids, closed codes,
// counts and bools only; a name, an MRN, a note or a free-text reason is
// refused.
func TestAuditSchemasAlerts(t *testing.T) {
	aid := uuid.NewString()
	good := map[string]map[string]any{
		"alert_settings.units_changed":      {"covered": 3, "units": 4},
		"alert_settings.rules_changed":      {"levels_changed": 2},
		"alert_settings.recipients_changed": {"recipients": 1, "queue": true, "requeued": 0},
		"alert.acknowledged":                {"alert_id": aid},
		"alert.dismissed":                   {"alert_id": aid, "reason": "already_known"},
		"alert.withdrawn":                   {"alert_id": aid, "reason": "excluded"},
		"alert.requeued":                    {"alert_ids": []any{aid}, "reason": "role_removed"},
		"alerts.read":                       {"alert_ids": []any{aid}, "status": "new", "source": true, "page": false},
		"emr.patient_matches_read":          {"match_ids": []any{aid}},
		"notification_settings.changed":     {"alert_popups": false, "correction_popups": true},
	}
	for action, p := range good {
		if err := audit.CheckActionPayload(action, p); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		bad := maps.Clone(p)
		bad["note"] = "SYNTHETIC NOTE"
		if err := audit.CheckActionPayload(action, bad); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s with a note: %v", action, err)
		}
	}
	for action, p := range map[string]map[string]any{
		"alert.dismissed": {"alert_id": aid, "reason": "the patient is fine"},
		"alert.requeued":  {"alert_ids": []any{"Synthetic Patient"}, "reason": "user_deactivated"},
		"alerts.read":     {"alert_ids": []any{aid}, "source": "Main record", "page": false},
	} {
		if err := audit.CheckActionPayload(action, p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s accepted %v: %v", action, p, err)
		}
	}
	if err := audit.CheckActionPayload("alert.requeued", map[string]any{"payload_refused": true}); err != nil {
		t.Errorf("refused shape: %v", err)
	}
}

// The analyst grant (plan Task 16): the change id, the channel and, for a
// removal, the reason; never a name, an email or free text.
func TestAnalystActionsSchemas(t *testing.T) {
	c := uuid.NewString()
	good := map[string]map[string]any{
		"analyst.granted": {"change_id": c, "via": "app"},
		"analyst.removed": {"change_id": c, "via": "cli", "reason": "clinician_removed"},
	}
	for action, p := range good {
		if err := audit.CheckActionPayload(action, p); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		for k, v := range map[string]any{"display_name": "Synthetic Staff", "person": "ada@example.test", "note": "asked"} {
			bad := maps.Clone(p)
			bad[k] = v
			if err := audit.CheckActionPayload(action, bad); err == nil {
				t.Errorf("%s with %s accepted", action, k)
			}
		}
	}
	for name, p := range map[string]struct {
		action string
		p      map[string]any
	}{
		"a removal without a reason": {"analyst.removed", map[string]any{"change_id": c, "via": "app"}},
		"a free-text reason":         {"analyst.removed", map[string]any{"change_id": c, "via": "app", "reason": "left the team"}},
		"a grant with a reason":      {"analyst.granted", map[string]any{"change_id": c, "via": "app", "reason": "removed"}},
		"a name as the change":       {"analyst.granted", map[string]any{"change_id": "Synthetic Staff", "via": "app"}},
		"another channel":            {"analyst.granted", map[string]any{"change_id": c, "via": "platform"}},
		"no change id":               {"analyst.granted", map[string]any{"via": "cli"}},
	} {
		if err := audit.CheckActionPayload(p.action, p.p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The exclusion purge, retention, legal holds and hospital removal (EMR
// PR 9): ids, counts and closed codes only; an MRN, a name or a free-text
// reference is refused.
func TestAuditSchemasEMRRetention(t *testing.T) {
	rid, pid := uuid.NewString(), uuid.NewString()
	counts := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	removed := counts("via", "cli")
	for _, k := range audit.HospitalDataRemovedKeys {
		removed[k] = 0
	}
	removed["observations"] = 7
	good := map[string]map[string]any{
		"emr.exclusion_purged": counts("request_id", rid, "patient_id", pid, "via", "loop", "observations", 2,
			"medications", 1, "encounters", 1, "raw_records", 6, "matches", 0, "findings", 3, "alerts", 2),
		"emr.exclusion_purge_held":     counts("request_id", rid, "patient_id", pid, "via", "route"),
		"emr.exclusion_purge_released": counts("request_ids", []any{rid}, "count", 1),
		"emr.retention_swept": counts("raw_payloads", 11, "findings", 0, "observations", 4, "medications", 0, "encounters", 0,
			"raw_records", 4, "alert_patients", 1, "alerts", 0, "consumed", 0, "patients", 1),
		"legal_hold.placed":         counts("hold_id", rid, "via", "cli"),
		"legal_hold.lifted":         counts("hold_id", rid, "via", "cli"),
		"emr.hospital_data_removed": removed,
		"emr.patient_excluded": counts("patient_id", pid, "reason", "objection", "via", "cli", "observations", 0,
			"medications", 0, "encounters", 0, "raw_records", 0, "matches", 0, "findings", 0, "request_id", rid, "held", true),
	}
	for action, p := range good {
		if err := audit.CheckActionPayload(action, p); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		for k, v := range map[string]any{"mrn": "SYN-6837", "display_name": "Synthetic Patient", "reference": "LH-2026-0042"} {
			bad := maps.Clone(p)
			bad[k] = v
			if err := audit.CheckActionPayload(action, bad); !errors.Is(err, audit.ErrSchema) {
				t.Errorf("%s with %s: %v", action, k, err)
			}
		}
	}
	for name, c := range map[string]struct {
		action string
		p      map[string]any
	}{
		"purged without counts":   {"emr.exclusion_purged", counts("request_id", rid, "patient_id", pid, "via", "loop")},
		"purged by a free via":    {"emr.exclusion_purged", counts("request_id", rid, "patient_id", "Synthetic Patient", "via", "loop")},
		"hold with a name":        {"legal_hold.placed", counts("hold_id", "Synthetic Person", "via", "cli")},
		"hold via the platform":   {"legal_hold.lifted", counts("hold_id", rid, "via", "platform")},
		"released over 500":       {"emr.exclusion_purge_released", counts("request_ids", make([]any, 501), "count", 501)},
		"removal of a free table": {"emr.hospital_data_removed", counts("via", "cli", "audit_log", 1)},
	} {
		if err := audit.CheckActionPayload(c.action, c.p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The platform admin routes' payloads (00037): a platform hold names its
// hospital and a code; the team and the e-mail setting carry ids and codes.
// A name, a reference, an email or a free code never fits.
func TestPlatformAdminPayloads(t *testing.T) {
	hold, hosp, lift, user := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	good := map[string]map[string]any{
		"legal_hold.placed":                {"hold_id": hold, "hospital_id": hosp, "reason": "regulator"},
		"legal_hold.lifted":                {"hold_id": hold, "hospital_id": hosp, "lift_id": lift},
		"legal_hold.lift_requested":        {"hold_id": hold, "hospital_id": hosp, "lift_id": lift, "reason": "case_closed"},
		"legal_hold.lift_cancelled":        {"hold_id": hold, "hospital_id": hosp, "lift_id": lift},
		"platform.team_role_changed":       {"user_id": user, "role": "auditor", "previous": "platform_admin"},
		"platform.team_member_removed":     {"user_id": user, "role": "auditor"},
		"platform.analytics_email_changed": {"mode": "critical", "previous": "off"},
	}
	for action, p := range good {
		if err := audit.CheckActionPayload(action, p); err != nil {
			t.Errorf("%s: %v", action, err)
		}
		for k, v := range map[string]any{"display_name": "Synthetic Okafor", "email": "synthetic.ok@havana.example",
			"reference": "LH-2026-7781", "note": "synthetic note"} {
			bad := maps.Clone(p)
			bad[k] = v
			if err := audit.CheckActionPayload(action, bad); !errors.Is(err, audit.ErrSchema) {
				t.Errorf("%s with %s: %v", action, k, err)
			}
		}
	}
	for name, c := range map[string]struct {
		action string
		p      map[string]any
	}{
		"platform hold without its hospital": {"legal_hold.placed", map[string]any{"hold_id": hold, "reason": "other"}},
		"cli hold with a hospital": {"legal_hold.placed", map[string]any{"hold_id": hold, "via": "cli",
			"hospital_id": hosp, "reason": "other"}},
		"free reason":      {"legal_hold.placed", map[string]any{"hold_id": hold, "via": "platform", "hospital_id": hosp, "reason": "Synthetic case"}},
		"lift via the cli": {"legal_hold.lift_requested", map[string]any{"hold_id": hold, "via": "cli", "hospital_id": hosp, "lift_id": lift, "reason": "other"}},
		"clinical role":    {"platform.team_role_changed", map[string]any{"user_id": user, "role": "clinician", "previous": "auditor"}},
		"name as the user": {"platform.team_member_removed", map[string]any{"user_id": "Synthetic Okafor", "role": "auditor"}},
		"free e-mail mode": {"platform.analytics_email_changed", map[string]any{"mode": "weekly", "previous": "off"}},
	} {
		if err := audit.CheckActionPayload(c.action, c.p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The patient lookup by MRN (00039): the patient id on a hit, the result
// otherwise; never the MRN typed, a name or a free-text window.
func TestPatientLookupPayloads(t *testing.T) {
	pid := uuid.NewString()
	for _, p := range []map[string]any{
		{"patient_id": pid},
		{"result": "miss"},
		{"result": "ambiguous"},
	} {
		if err := audit.CheckActionPayload("patients.lookup", p); err != nil {
			t.Errorf("%v: %v", p, err)
		}
	}
	for _, w := range []string{"minute", "day", "hospital_day"} {
		if err := audit.CheckActionPayload("patients.lookup_limit_reached", map[string]any{"window": w}); err != nil {
			t.Errorf("%s: %v", w, err)
		}
	}
	for name, c := range map[string]struct {
		action string
		p      map[string]any
	}{
		"a hit as a result":        {"patients.lookup", map[string]any{"result": "hit"}},
		"a hit and a result":       {"patients.lookup", map[string]any{"result": "miss", "patient_id": pid}},
		"nothing":                  {"patients.lookup", map[string]any{}},
		"ambiguous with a patient": {"patients.lookup", map[string]any{"result": "ambiguous", "patient_id": pid}},
		"the mrn typed":            {"patients.lookup", map[string]any{"result": "miss", "mrn": "SYN-LKP-7Q4X-M2"}},
		"a name":                   {"patients.lookup", map[string]any{"patient_id": pid, "full_name": "Synthetic Odalys Brennecke"}},
		"an mrn as the patient":    {"patients.lookup", map[string]any{"patient_id": "SYN-LKP-7Q4X-M2"}},
		"free result":              {"patients.lookup", map[string]any{"result": "found"}},
		"free window":              {"patients.lookup_limit_reached", map[string]any{"window": "hour"}},
		"limit without a window":   {"patients.lookup_limit_reached", map[string]any{}},
	} {
		if err := audit.CheckActionPayload(c.action, c.p); !errors.Is(err, audit.ErrSchema) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
