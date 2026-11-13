package audit_test

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// The vocabulary mhai-web's audit screens accept, vendored from mhai-web
// (Plan 4 spec §5.3). Refreshed by hand with the mhai-web change that adds
// a kind; pending_in_web lists the kinds this backend sends that mhai-web
// has yet to ship.
type vocabulary struct {
	Source struct {
		Repo   string   `json:"repo"`
		Commit string   `json:"commit"`
		Files  []string `json:"files"`
	} `json:"source"`
	Types       []string            `json:"types"`
	Outcomes    []string            `json:"outcomes"`
	Flags       []string            `json:"flags"`
	TargetKinds []string            `json:"target_kinds"`
	Facts       map[string]rule     `json:"facts"`
	Filters     map[string]rule     `json:"filters"`
	Kinds       map[string]kindRule `json:"kinds"`
	Pending     []pendingKind       `json:"pending_in_web"`
	kinds       map[string]kindRule // Kinds and Pending together
	pending     map[string]bool     // the pending kinds
	types       map[string]bool     // Types as a set
}

type rule struct {
	Shape  string   `json:"shape"`
	Values []string `json:"values"`
}

type kindRule struct {
	Type    string   `json:"type"`
	Targets []string `json:"targets"`
	Note    bool     `json:"note"`
}

type pendingKind struct {
	Kind string `json:"kind"`
	kindRule
	For []string `json:"for"`
}

func loadVocabulary(t *testing.T) *vocabulary {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "testdata", "web-audit-vocabulary.json"))
	if err != nil {
		t.Fatalf("read the vendored vocabulary: %v", err)
	}
	var v vocabulary
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode the vocabulary: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(v.Source.Commit) || v.Source.Repo == "" || len(v.Source.Files) == 0 {
		t.Fatalf("the vocabulary doesn't name the mhai-web commit and files it was copied from: %+v", v.Source)
	}
	if len(v.Types) == 0 || len(v.Kinds) == 0 || len(v.Facts) == 0 || len(v.Outcomes) == 0 || len(v.Filters) == 0 {
		t.Fatal("the vocabulary is missing a list")
	}
	v.kinds, v.pending, v.types = map[string]kindRule{}, map[string]bool{}, map[string]bool{}
	for _, ty := range v.Types {
		v.types[ty] = true
	}
	for k, r := range v.Kinds {
		v.kinds[k] = r
	}
	for _, p := range v.Pending {
		if _, dup := v.kinds[p.Kind]; dup {
			t.Errorf("pending kind %q is already in mhai-web's list: drop it from pending_in_web", p.Kind)
		}
		v.kinds[p.Kind] = p.kindRule
		v.pending[p.Kind] = true
	}
	return &v
}

func in(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var (
	webID     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	webCode   = regexp.MustCompile(`^[a-z][a-z_]{0,39}$`)
	webView   = regexp.MustCompile(`^[a-z][a-z_-]{0,39}$`)
	webHash   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	webTime   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T([01]\d|2[0-3]):[0-5]\d(:[0-5]\d(\.\d{1,9})?)?(Z|[+-]\d{2}:\d{2})$`)
	pseudonym = regexp.MustCompile(`^P-[A-Z0-9]{4,12}$`)
	// mhai-web's PSEUDONYM_IN, without its lookahead: one anywhere in a string.
	pseudonymIn = regexp.MustCompile(`(^|[^A-Za-z0-9])P-[A-Z0-9]{4,12}([^A-Za-z0-9]|$)`)
)

// fits reports whether v has the shape mhai-web's reader checks for r.
func (voc *vocabulary) fits(r rule, v any) bool {
	str, isStr := v.(string)
	num, isNum := v.(float64)
	whole := isNum && num == float64(int64(num)) && num <= 1<<53-1
	switch r.Shape {
	case "enum":
		return isStr && in(r.Values, str)
	case "code":
		return isStr && webCode.MatchString(str)
	case "view":
		return isStr && webView.MatchString(str)
	case "id":
		return isStr && webID.MatchString(str)
	case "hash":
		return isStr && webHash.MatchString(str)
	case "time":
		if !isStr || !webTime.MatchString(str) {
			return false
		}
		_, err := time.Parse(time.RFC3339Nano, str)
		return err == nil
	case "count":
		return whole && num >= 0
	case "positive":
		return whole && num > 0
	case "bool":
		_, ok := v.(bool)
		return ok
	case "types":
		l, ok := v.([]any)
		if !ok {
			return false
		}
		for _, x := range l {
			s, ok := x.(string)
			if !ok || !voc.types[s] {
				return false
			}
		}
		return true
	case "id_or_marker":
		if isStr {
			return webID.MatchString(str)
		}
		m, ok := v.(map[string]any)
		return ok && len(m) == 1 && m["kind"] == "patient_id"
	case "filters":
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for k, x := range m {
			fr, known := voc.Filters[k]
			if !known || !voc.fits(fr, x) {
				return false
			}
		}
		return true
	}
	return false
}

// hasPseudonym reports whether any string in v (keys included), at any
// depth, is or holds something of the pseudonym's shape.
func hasPseudonym(v any) bool {
	switch x := v.(type) {
	case string:
		return pseudonym.MatchString(x) || pseudonymIn.MatchString(x)
	case []any:
		for _, e := range x {
			if hasPseudonym(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if hasPseudonym(k) || hasPseudonym(e) {
				return true
			}
		}
	}
	return false
}

// checkMapped holds one mapped event against the vocabulary.
func (voc *vocabulary) checkMapped(t *testing.T, action string, m audit.Mapped) {
	t.Helper()
	k, ok := voc.kinds[m.Kind]
	switch {
	case !voc.types[m.Type]:
		t.Errorf("%s: type %q is not mhai-web's", action, m.Type)
	case !ok:
		t.Errorf("%s: kind %q is not mhai-web's, nor pending", action, m.Kind)
	case k.Type != m.Type:
		t.Errorf("%s: kind %q is of type %q in mhai-web, sent as %q", action, m.Kind, k.Type, m.Type)
	}
	if !in(voc.Outcomes, m.Outcome) {
		t.Errorf("%s: outcome %q", action, m.Outcome)
	}
	if m.Flag != nil && !in(voc.Flags, *m.Flag) {
		t.Errorf("%s: flag %q", action, *m.Flag)
	}
	if m.Target != nil {
		if !in(k.Targets, m.Target.Kind) || !in(voc.TargetKinds, m.Target.Kind) {
			t.Errorf("%s: kind %q may not name a %q target", action, m.Kind, m.Target.Kind)
		}
		if !webID.MatchString(m.Target.ID) {
			t.Errorf("%s: target id is not an opaque id", action)
		}
	}
	for key, v := range m.Facts {
		r, known := voc.Facts[key]
		if !known {
			t.Errorf("%s: facts key %q is not mhai-web's", action, key)
			continue
		}
		if !voc.fits(r, v) {
			t.Errorf("%s: facts.%s doesn't have mhai-web's shape (%s)", action, key, r.Shape)
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if hasPseudonym(back) {
		t.Errorf("%s: the mapped event holds something of the pseudonym's shape", action)
	}
}

// withheld reports whether a row is sent as the bare undisplayable entry.
func withheld(e audit.Entry) bool {
	return e.Type == audit.UnknownType && e.Kind == audit.UnknownKind
}

// outboxActions are the actions intelligence's outbox may carry, as the
// migrations' CHECKs name them (00008, widened in 00012).
func outboxActions(t *testing.T) []string {
	t.Helper()
	inList := regexp.MustCompile(`action IN \(([^)]*)\)`)
	quoted := regexp.MustCompile(`'([a-z][a-z0-9_.]*)'`)
	seen := map[string]bool{}
	for _, f := range []string{"00008_intelligence_worker.sql", "00012_jobs.sql"} {
		raw, err := os.ReadFile(filepath.Join("..", "db", "migrations", f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range inList.FindAllStringSubmatch(string(raw), -1) {
			for _, q := range quoted.FindAllStringSubmatch(m[1], -1) {
				seen[q[1]] = true
			}
		}
	}
	var out []string
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	if len(out) < 5 {
		t.Fatalf("found only %v in the outbox CHECKs: the parse proves nothing", out)
	}
	return out
}

// platformAppendActions are the actions the latest platform_append allows:
// every migration's Up section is read in order, and the last CREATE POLICY
// platform_append wins (00014 made it; 00018 and 00020 widened it).
func platformAppendActions(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "db", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	re := regexp.MustCompile(`(?s)CREATE POLICY platform_append ON audit_log.*?action IN \(([^)]*)\)`)
	var last, from string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up := strings.SplitN(string(raw), "-- +goose Down", 2)[0]
		for _, m := range re.FindAllStringSubmatch(up, -1) {
			last, from = m[1], filepath.Base(f)
		}
	}
	if last == "" {
		t.Fatal("no migration has a platform_append with an action list")
	}
	if from < "00022" {
		t.Fatalf("the latest platform_append is %s's; 00022 widened it", from)
	}
	var out []string
	for _, q := range regexp.MustCompile(`'([a-z][a-z0-9_.]*)'`).FindAllStringSubmatch(last, -1) {
		out = append(out, q[1])
	}
	sort.Strings(out)
	return out
}

// TestEveryActionHasCatalogueRow (C21): every action core writes, every
// outbox action and every registered schema has a catalogue row, and every
// row is a registered action.
func TestEveryActionHasCatalogueRow(t *testing.T) {
	for a, at := range collectActions(t) {
		if _, ok := audit.Catalogue[a]; !ok {
			t.Errorf("no catalogue row for %s (%s)", a, at)
		}
	}
	for _, a := range outboxActions(t) {
		if _, ok := audit.Catalogue[a]; !ok {
			t.Errorf("no catalogue row for outbox action %s", a)
		}
	}
	for _, a := range platformAppendActions(t) {
		if _, ok := audit.Catalogue[a]; !ok {
			t.Errorf("no catalogue row for platform action %s", a)
		}
		if !audit.Registered(a) {
			t.Errorf("platform action %s has no payload schema", a)
		}
	}
	if got := platformAppendActions(t); len(got) != 20 || !slices.Contains(got, "audit.patients_viewed") || !slices.Contains(got, "emr.rule_set_switched") || !slices.Contains(got, "hospital.model_policy_changed") ||
		!slices.Contains(got, "emr.connection_added") || !slices.Contains(got, "emr.source_resumed") ||
		!slices.Contains(got, "analytics.followed_up") || !slices.Contains(got, "analytics.follow_up_edited") ||
		!slices.Contains(got, "analytics.follow_up_undone") {
		t.Errorf("platform_append allows %v, want spec §5.5's eight, hospital.model_policy_changed (00018), the six EMR actions (00020) "+
			"the three follow-up actions (00022), emr.rule_set_switched (00027) and audit.patients_viewed (00028)", got)
	}
	for _, a := range audit.RegisteredActions() {
		if strings.HasPrefix(a, "test.") {
			continue // the chain tests' own actions (integration builds)
		}
		if _, ok := audit.Catalogue[a]; !ok {
			t.Errorf("registered action %s has no catalogue row", a)
		}
	}
	for a, e := range audit.Catalogue {
		if !audit.Registered(a) {
			t.Errorf("catalogue row %s names no registered action", a)
		}
		if e.Type == "" || e.Kind == "" {
			t.Errorf("catalogue row %s has no type or kind", a)
		}
		fields := audit.SchemaFields(a)
		for from := range e.Facts {
			if !in(fields, from) {
				t.Errorf("%s maps payload key %q, which its schema doesn't have", a, from)
			}
		}
		if withheld(e) && (e.Target != nil || e.Outcome != nil || e.Flag != nil || len(e.Facts) > 0) {
			t.Errorf("%s is withheld but derives fields", a)
		}
	}
}

// TestCatalogueUsesOnlyWebVocabulary (C21): every type, kind, target kind
// and facts key a row can emit is mhai-web's (or pending there), and the
// pending list is exactly the kinds this catalogue uses that mhai-web lacks.
func TestCatalogueUsesOnlyWebVocabulary(t *testing.T) {
	voc := loadVocabulary(t)
	used := map[string]bool{}
	for a, e := range audit.Catalogue {
		if withheld(e) {
			continue
		}
		k, ok := voc.kinds[e.Kind]
		if !ok {
			t.Errorf("%s: kind %q is neither mhai-web's nor pending", a, e.Kind)
			continue
		}
		if k.Type != e.Type || !voc.types[e.Type] {
			t.Errorf("%s: %s/%s, but mhai-web files %s under %q", a, e.Type, e.Kind, e.Kind, k.Type)
		}
		if voc.pending[e.Kind] {
			used[e.Kind] = true
		}
		for _, to := range e.Facts {
			if _, ok := voc.Facts[to]; !ok {
				t.Errorf("%s: facts key %q is not mhai-web's", a, to)
			}
		}
	}
	for _, p := range voc.Pending {
		if !used[p.Kind] {
			t.Errorf("pending kind %q is used by no catalogue row", p.Kind)
		}
		for _, a := range p.For {
			if e, ok := audit.Catalogue[a]; !ok || e.Kind != p.Kind {
				t.Errorf("pending kind %q names %s, which the catalogue doesn't send as it", p.Kind, a)
			}
		}
	}
	// What the rows emit, over generated payloads, has mhai-web's shapes.
	fuzzCatalogue(t, voc, 200)
}

// TestCatalogueNeverEmitsPseudonymShape (C22): over payloads of every
// schema's shapes, pseudonym-shaped values among them, no mapped event holds
// a string of the pseudonym's shape, and no mapped facts key's schema
// accepts one at all.
func TestCatalogueNeverEmitsPseudonymShape(t *testing.T) {
	voc := loadVocabulary(t)
	for a, e := range audit.Catalogue {
		for from := range e.Facts {
			for _, p := range pseudonyms {
				for _, v := range []any{p, []any{p}, map[string]any{"hospital": p}, map[string]any{"person": p}, map[string]any{"q": p}} {
					if audit.FieldAccepts(a, from, v) {
						t.Errorf("%s: payload key %q (mapped to facts) accepts %#v", a, from, v)
					}
				}
			}
		}
	}
	fuzzCatalogue(t, voc, 2000)
}

var pseudonyms = []string{"P-ABCD", "P-7F3K2", "P-ABCDEFGHIJKL", "P-0000", "x P-ABCD1 y", "req-P-7F3K2", "P-7F3K2-x"}

// candidates are the values the generator draws payloads from: every code
// the schemas know, ids, counts, times, and pseudonym-shaped strings.
func candidates() []any {
	ids := []any{"6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11", "0b6c6d4e-8a9f-4f6e-b2d3-1c2a3b4c5d6e"}
	var out []any
	out = append(out, ids...)
	for _, p := range pseudonyms {
		out = append(out, p)
	}
	for _, s := range strings.Fields(`events named_access event request export clearance pseudonym person schedule
		running intact broken error timed_out unreachable unexpected_reply self_approval forbidden already_decided
		approve reject withdraw not_supported needs_rewording unsafe out_of_scope skipped applied run_query
		approve_correction user_deactivated hospital_deactivated account_unusable no_longer_needed cant_succeed other
		routine urgent emergency cli setup_incomplete password_changed locked device bad_credentials not_platform
		platform_account bad_current weak token_attempts account_attempts token_mismatch otp backup none step_up pwd
		planned emergency added removed PASS REGENERATE ESCALATE low mid high unauthorized invalid_request succeeded
		refused failed platform via msg-1 kid-2026 core pipeline.run_query nli 1 42 9007199254740991 0
		2026-11-03T06:00:00Z 2026-11-03T06:00:00.123+01:00 v1.2
		fhir_r4 stub ok sign_in_refused base_url private_key platform_resume demo prevalence month:2026-10-01 affected minute day`) {
		out = append(out, s)
	}
	for _, ty := range audit.AuditTypes {
		out = append(out, ty)
	}
	for _, c := range authz.ErrorCodes() {
		out = append(out, c.Code)
	}
	for _, n := range []float64{0, 1, 2, 49, 50, 51, 1000, 65536, 1e15, -1, 1.5} {
		out = append(out, n)
	}
	out = append(out, true, false, nil,
		[]any{"pwd", "otp"}, []any{"audit", "security"}, []any{"base_url", "private_key"}, []any{"P-ABCD12"}, []any{ids[0]}, []any{},
		map[string]any{}, map[string]any{"hospital": "platform"},
		map[string]any{"hospital": ids[0], "person": ids[1], "type": []any{"audit", "review"}, "outcome": "refused",
			"flagged": true, "from": "2026-11-01T00:00:00Z", "to": "2026-11-03T00:00:00Z"},
		map[string]any{"hospital": "P-ABCD12"}, map[string]any{"person": "P-7F3K2"}, map[string]any{"q": "P-ABCD12"},
		map[string]any{"q": ids[0]}, map[string]any{"type": []any{"P-ABCD12"}},
		map[string]any{"nli": "v1"}, map[string]any{"nli": "P-ABCD12"},
		map[string]any{"sex": "female"}, map[string]any{"location": "P-ABCD12"})
	return out
}

// fuzzCatalogue maps n generated events per catalogue row and holds each
// result against the vocabulary and the pseudonym rule.
func fuzzCatalogue(t *testing.T, voc *vocabulary, n int) {
	t.Helper()
	rng := rand.New(rand.NewSource(20261103))
	pool := candidates()
	entities := []string{"", "session", "hospital", "query", "correction", "job", "audit_run", "audit_list", "audit_search", "user", "operation", "emr_connection"}
	entityIDs := append([]string{"", "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11", "pipeline.run_query", "has space"}, pseudonyms...)
	hospitals := []string{"", "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11"}
	mappedOnce := map[string]bool{}
	defer func() {
		for a, e := range audit.Catalogue {
			if withheld(e) || mappedOnce[a] {
				continue
			}
			t.Errorf("%s never mapped over %d generated events: the fuzz proves nothing for it", a, n)
		}
	}()
	actions := make([]string, 0, len(audit.Catalogue))
	for a := range audit.Catalogue {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	for _, a := range actions {
		e := audit.Catalogue[a]
		fields := audit.SchemaFields(a)
		sort.Strings(fields)
		required := audit.SchemaRequired(a)
		accepted := map[string][]any{}
		for _, f := range fields {
			for _, c := range pool {
				if audit.FieldAccepts(a, f, c) {
					accepted[f] = append(accepted[f], c)
				}
			}
		}
		for from := range e.Facts {
			if len(accepted[from]) == 0 {
				t.Errorf("%s: no generated value fits mapped key %q: the fuzz proves nothing for it", a, from)
			}
		}
		for _, f := range required {
			if len(accepted[f]) == 0 {
				t.Fatalf("%s: no generated value fits required key %q", a, f)
			}
		}
		shapes := audit.SchemaShapes(a)
		for i := 0; i < n; i++ {
			payload := map[string]any{}
			// An action with shapes: one shape per event, its required
			// keys always and its other keys at random.
			keys, must := fields, required
			if len(shapes) > 0 {
				sh := shapes[rng.Intn(len(shapes))]
				keys, must = sh.Keys, append(slices.Clone(required), sh.Required...)
			}
			for _, f := range fields {
				vals := accepted[f]
				if len(vals) == 0 || !in(keys, f) || (!in(must, f) && rng.Intn(2) == 0) {
					continue
				}
				payload[f] = vals[rng.Intn(len(vals))]
			}
			ev := audit.Event{
				Action: a, Payload: payload,
				Entity:     entities[rng.Intn(len(entities))],
				EntityID:   entityIDs[rng.Intn(len(entityIDs))],
				HospitalID: hospitals[rng.Intn(len(hospitals))],
			}
			if e.When != nil && i%2 == 1 {
				// A gated row (the job batch rows want no entity): half
				// the events have none, so the gate's mapped side is
				// reached as well as its refused one.
				ev.Entity, ev.EntityID = "", ""
			}
			if err := audit.CheckActionPayload(a, payload); err != nil {
				t.Fatalf("%s: the generator built a payload its schema refuses: %v", a, err)
			}
			m, ok := audit.Map(ev)
			// A schema-valid payload maps exactly when its row is not
			// withheld and takes this row of the action: the guards
			// (pseudonym, target id) never fire on what the schemas let
			// through, so a refusal here is a catalogue bug.
			want := !withheld(e) && (e.When == nil || e.When(ev))
			if ok != want {
				t.Fatalf("%s: Map ok=%v, want %v, for payload %v entity %q/%q", a, ok, want, payload, ev.Entity, ev.EntityID)
			}
			if !ok {
				continue
			}
			mappedOnce[a] = true
			if m.Type != e.Type || m.Kind != e.Kind {
				t.Errorf("%s mapped to %s/%s, its row says %s/%s", a, m.Type, m.Kind, e.Type, e.Kind)
			}
			voc.checkMapped(t, a, m)
			for key, v := range m.Facts {
				var from string
				for f, to := range e.Facts {
					if to == key {
						from = f
					}
				}
				if from == "" {
					t.Errorf("%s: facts key %q comes from no mapped payload key", a, key)
					continue
				}
				if b1, _ := json.Marshal(v); string(b1) != mustJSON(t, payload[from]) {
					t.Errorf("%s: facts.%s is not the payload's %s", a, key, from)
				}
			}
			if t.Failed() {
				return
			}
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestUnmappedActionSendsNoPayload: an action with no row, a withheld row
// and a payload outside its schema are all sent as the bare entry: id, seq,
// hash, at, security/unknown, and nothing of the payload, entity or actor.
func TestUnmappedActionSendsNoPayload(t *testing.T) {
	const marker = "SYNTHETIC-UNMAPPED-91c2"
	at := time.Date(2026, 11, 3, 6, 0, 0, 123456000, time.UTC)
	hash := strings.Repeat("a", 64)
	for name, ev := range map[string]audit.Event{
		"no row":            {Action: "test.made_up", Entity: marker, EntityID: marker, Payload: map[string]any{"note": marker}},
		"withheld row":      {Action: "intelligence.granted", Entity: "operation", EntityID: "pipeline.run_query", Payload: map[string]any{"operation": "pipeline.run_query", "jti": marker, "service": "core"}},
		"payload off shape": {Action: "audit.searched", Payload: map[string]any{"searched_kind": marker}},
		"extra key":         {Action: "audit.check_outcome", Payload: map[string]any{"run_id": "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11", "note": marker}},
	} {
		ev.ActorID, ev.RequestID = "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11", marker
		if m, ok := audit.Map(ev); ok {
			t.Errorf("%s: mapped to %+v", name, m)
		}
		b := audit.Unmapped(audit.Row{Seq: 42, Hash: hash, At: at, Event: ev})
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"id": "42", "seq": float64(42), "hash": hash, "at": "2026-11-03T06:00:00.123456Z",
			"type": "security", "kind": "unknown"}
		if len(got) != len(want) {
			t.Errorf("%s: bare entry has keys %v", name, got)
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("%s: %s = %v, want %v", name, k, got[k], w)
			}
		}
		if strings.Contains(string(raw), marker) {
			t.Errorf("%s: the bare entry carries a value of the row", name)
		}
	}
	// A known action with a fitting payload maps.
	m, ok := audit.Map(audit.Event{Action: "audit.searched", Payload: map[string]any{"searched_kind": "request"}})
	if !ok || m.Kind != "audit_searched" || m.Facts["searched_kind"] != "request" || m.Outcome != "succeeded" {
		t.Errorf("audit.searched mapped to %+v, %v", m, ok)
	}
}

// TestPerJobRowsAreNotPlatformBatches (ruling R24): a job.retried or
// job.cancelled row naming one job (core's dead-letter re-mint, the
// sweeper's cancels) is a system row and is withheld; only the platform's
// one-per-call row, with job_count and no job, is "Retried/Cancelled
// failed jobs".
func TestPerJobRowsAreNotPlatformBatches(t *testing.T) {
	job := "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11"
	perJob := map[string]audit.Event{
		"job.retried": {Action: "job.retried", Entity: "job", EntityID: job,
			Payload: map[string]any{"job_id": job, "msg_id": "msg-1"}},
		"job.cancelled": {Action: "job.cancelled", Entity: "job", EntityID: job,
			Payload: map[string]any{"job_id": job, "type": "approve_correction", "reason": "user_deactivated"}},
	}
	for a, ev := range perJob {
		if err := audit.CheckActionPayload(a, ev.Payload); err != nil {
			t.Fatalf("%s: the per-job row doesn't fit its schema: %v", a, err)
		}
		if m, ok := audit.Map(ev); ok {
			t.Errorf("%s per-job row mapped to %s/%s", a, m.Type, m.Kind)
		}
		e := audit.Catalogue[a]
		if e.When == nil {
			t.Fatalf("%s has no batch gate", a)
		}
		for name, x := range map[string]audit.Event{
			"per-job":             ev,
			"job id with a count": {Action: a, Payload: map[string]any{"job_id": job, "job_count": 2.0}},
			"entity with a count": {Action: a, Entity: "job", EntityID: job, Payload: map[string]any{"job_count": 2.0}},
			"no count":            {Action: a, Payload: map[string]any{}},
		} {
			if e.When(x) {
				t.Errorf("%s: the %s row passes the batch gate", a, name)
			}
		}
		if e.When(audit.Event{Action: a, Payload: map[string]any{"job_count": 3.0, "type": "run_query"}}) {
			t.Errorf("%s: a count with a per-job type passes the batch gate", a)
		}
		if !e.When(audit.Event{Action: a, Payload: map[string]any{"job_count": 3.0}}) {
			t.Errorf("%s: the batch row fails the batch gate", a)
		}
	}
}

// TestMapGuardsRefusePseudonymShapes: whatever a row derives, a target id
// or a facts value of the pseudonym's shape (alone or inside a string), or
// a target id that isn't opaque, sends the bare entry. The catalogue's own
// rows can't produce one (TestCatalogueNeverEmitsPseudonymShape); this
// holds the runtime guard on its own.
func TestMapGuardsRefusePseudonymShapes(t *testing.T) {
	run := "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11"
	outcome := audit.Event{Action: "audit.check_outcome", Payload: map[string]any{"run_id": run}}
	target := func(id string) audit.Entry {
		return audit.Entry{Type: "audit", Kind: "audit_check_outcome",
			Target: func(audit.Event) *audit.Target { return &audit.Target{Kind: "run", ID: id} }}
	}
	for _, id := range []string{"P-ABCD12", "P-7F3K2", "req-P-7F3K2", "has space", ""} {
		if m, ok := audit.MapRow(target(id), outcome); ok {
			t.Errorf("target id %q mapped: %+v", id, m)
		}
	}
	if m, ok := audit.MapRow(target(run), outcome); !ok || m.Target == nil || m.Target.ID != run {
		t.Errorf("a uuid target was refused: %+v %v", m, ok)
	}

	// msg_id is a token its schema lets take the pseudonym's shape; a row
	// that (wrongly) mapped it to facts is stopped at the door.
	facts := audit.Entry{Type: "platform", Kind: "jobs_retried", Facts: map[string]string{"msg_id": "searched"}}
	for _, msg := range []string{"P-ABCD12", "P-7F3K2", "req-P-7F3K2", "P-ABCDEFGHIJKL"} {
		ev := audit.Event{Action: "job.retried", Payload: map[string]any{"job_id": run, "msg_id": msg}}
		if err := audit.CheckActionPayload("job.retried", ev.Payload); err != nil {
			t.Fatalf("msg_id %q: %v", msg, err)
		}
		if m, ok := audit.MapRow(facts, ev); ok {
			t.Errorf("facts value %q mapped: %+v", msg, m)
		}
	}
	m, ok := audit.MapRow(facts, audit.Event{Action: "job.retried", Payload: map[string]any{"job_id": run, "msg_id": "msg-1"}})
	if !ok || m.Facts["searched"] != "msg-1" {
		t.Errorf("a plain facts value was refused: %+v %v", m, ok)
	}
}

// Plan 4 Task 12 (carry item 6): the platform's batch rows fit the widened
// schemas and map to jobs_retried and jobs_cancelled with facts.job_count
// only (never the reason, which mhai-web has no key for); the per-job
// system rows keep their shape and stay withheld (R24).
func TestBatchJobRowsMap(t *testing.T) {
	for _, c := range []struct {
		action, kind string
		payload      map[string]any
	}{
		{"job.retried", "jobs_retried", map[string]any{"job_count": 3.0}},
		{"job.cancelled", "jobs_cancelled", map[string]any{"job_count": 2.0, "reason": "other"}},
	} {
		if err := audit.CheckActionPayload(c.action, c.payload); err != nil {
			t.Fatalf("%s batch row: %v", c.action, err)
		}
		m, ok := audit.Map(audit.Event{Action: c.action, Payload: c.payload})
		if !ok || m.Type != "platform" || m.Kind != c.kind || len(m.Facts) != 1 || m.Facts["job_count"] != c.payload["job_count"] {
			t.Errorf("%s mapped to %+v, %v", c.action, m, ok)
		}
	}
	job := "6f1c1c3e-2c0b-4c55-9a52-0d2e5b0b7a11"
	for name, p := range map[string]map[string]any{
		"retried, empty":              {},
		"retried, mixed":              {"job_count": 1.0, "job_id": job, "msg_id": "msg-1"},
		"retried, half a per-job":     {"job_id": job},
		"cancelled, empty":            {},
		"cancelled, mixed":            {"job_count": 1.0, "job_id": job, "type": "run_query", "reason": "other"},
		"cancelled, count and type":   {"job_count": 1.0, "type": "run_query", "reason": "other"},
		"retried, 0 jobs":             {"job_count": 0.0},
		"retried, 51 jobs":            {"job_count": 51.0},
		"retried, a note":             {"job_count": 1.0, "note": "synthetic"},
		"cancelled, a note":           {"job_count": 1.0, "reason": "other", "note": "synthetic"},
		"cancelled, no reason":        {"job_count": 1.0},
		"cancelled, a made-up reason": {"job_count": 1.0, "reason": "bored"},
	} {
		action := "job.retried"
		if strings.HasPrefix(name, "cancelled") {
			action = "job.cancelled"
		}
		if err := audit.CheckActionPayload(action, p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
