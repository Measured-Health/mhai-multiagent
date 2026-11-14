package patients

import "net/http"

// Handler is the lookup's handler, for tests that serve it with a
// principal of their own in the context (the guard's role and two-step
// checks are authz's, pinned by TestLookupRouteRule).
func Handler(d Deps) http.Handler { return http.HandlerFunc(d.lookup) }
