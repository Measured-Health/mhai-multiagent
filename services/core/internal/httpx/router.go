package httpx

import (
	"net/http"
	"path"
	"strings"
)

// BasePath is where every API route is mounted.
const BasePath = "/api/v1"

// MaxBody is the largest request body accepted, in bytes.
const MaxBody = 64 << 10

// Router collects the /api/v1 routes. Handler wraps them in the request
// id, BFF credential and body limit; NATSHandler in the request id, the
// NATS mark (ViaNATS) and body limit. There is no way to serve a route
// without one or the other.
//
// Application routes register through authz.Route, never Handle directly:
// authz.Route puts the route behind the principal middleware and in the
// permission table, and TestEveryRouteInTable fails on any pattern here
// that the table lacks.
type Router struct {
	mux      *http.ServeMux
	patterns []string
}

// NewRouter returns an empty router.
func NewRouter() *Router {
	return &Router{mux: http.NewServeMux()}
}

// Handle registers h for method and pattern, a path under /api/v1 such as
// "/auth/login" or "/users/{id}".
func (rt *Router) Handle(method, pattern string, h http.Handler) {
	if method == "" || !strings.HasPrefix(pattern, "/") {
		panic("httpx: Handle needs a method and a pattern starting with /")
	}
	rt.mux.Handle(method+" "+BasePath+pattern, h)
	rt.patterns = append(rt.patterns, method+" "+BasePath+pattern)
}

// Patterns returns every "METHOD /api/v1/path" registered, in order.
func (rt *Router) Patterns() []string {
	return append([]string(nil), rt.patterns...)
}

// Handler is the router behind RequestID → BFF(serviceKeys) → body limit.
// An unknown route gets 404 not_found as a problem body.
func (rt *Router) Handler(serviceKeys []string) http.Handler {
	return RequestID(BFF(serviceKeys)(limitBody(rt.routes())))
}

// NATSHandler is the same router behind RequestID → requireNATS → body
// limit: the HTTP chain with the BFF key check replaced by the NATS mark
// (ViaNATS, set by internal/natsapi only). Everything after it, the
// guard included, is the same handler as over HTTP.
func (rt *Router) NATSHandler() http.Handler {
	return RequestID(requireNATS(limitBody(rt.routes())))
}

func (rt *Router) routes() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An unclean path ("//", "." or "..") is refused, not redirected.
		if !cleanPath(r.URL.Path) {
			Error(w, r, "not_found")
			return
		}
		if _, pattern := rt.mux.Handler(r); pattern == "" {
			Error(w, r, "not_found")
			return
		}
		rt.mux.ServeHTTP(w, r)
	})
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// cleanPath reports whether p is already clean (a trailing slash allowed).
func cleanPath(p string) bool {
	c := path.Clean(p)
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return p == c
}
