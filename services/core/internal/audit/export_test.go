package audit

import "encoding/json"

// CheckPayload runs Write's checks on e (action, ids, sensitive keys, the
// schema) without a transaction.
func CheckPayload(e Event) error {
	_, _, _, err := prepare(e)
	return err
}

// RegisterOpenForTest registers each test action with a schema that takes
// any value under keys, none required, for the chain tests whose payloads
// exercise hashing rather than a real action. Test builds only.
func RegisterOpenForTest(keys []string, actions ...string) {
	any := func(any) bool { return true }
	for _, a := range actions {
		if Registered(a) {
			continue
		}
		f := map[string]Check{}
		for _, k := range keys {
			f[k] = any
		}
		Register(a, Schema{Fields: f})
	}
}

// RegisteredActions lists every action with a schema.
func RegisteredActions() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for a := range registry {
		out = append(out, a)
	}
	return out
}

// SchemaFields lists the keys action's schema allows.
func SchemaFields(action string) []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	var out []string
	for k := range registry[action].Fields {
		out = append(out, k)
	}
	return out
}

// SchemaRequired lists the keys action's schema requires.
func SchemaRequired(action string) []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return append([]string(nil), registry[action].Required...)
}

// SchemaShapes lists action's shapes: each one's keys and required keys.
func SchemaShapes(action string) []Shape {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return append([]Shape(nil), registry[action].Shapes...)
}

// FieldAccepts reports whether action's schema accepts v under key, as
// Write would see v after a JSON round trip.
func FieldAccepts(action, key string, v any) bool {
	registryMu.RLock()
	c := registry[action].Fields[key]
	registryMu.RUnlock()
	if c == nil {
		return false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var back any
	if err := json.Unmarshal(raw, &back); err != nil {
		return false
	}
	return c(back)
}

// MapRow applies row to e, as Map applies e's own row: for testing the
// guards against rows the catalogue doesn't have.
func MapRow(row Entry, e Event) (Mapped, bool) { return mapRow(row, e) }

// Classify is what the list's filters see of e (reader.go).
func Classify(e Event) Class { return classify(e) }

// GateHolds evaluates the reader's gate for e's action, as the list's SQL
// applies it; ok is false when the reader has no gate for it.
func GateHolds(e Event) (holds, ok bool) {
	g, ok := gates[e.Action]
	if !ok {
		return false, false
	}
	g.actions = []string{e.Action}
	return g.holds(e), true
}

// SetWalkPageForTest sets the chain walk's page size and returns a func
// that restores it. Test builds only.
func SetWalkPageForTest(n int) func() {
	old := walkPage
	walkPage = n
	return func() { walkPage = old }
}
