package audit

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
