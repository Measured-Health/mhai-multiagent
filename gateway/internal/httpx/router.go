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
// id, BFF credential and body limit; there is no way to serve a route
// without them.
type Router struct {
	mux *http.ServeMux
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
}

// Handler is the router behind RequestID → BFF(serviceKeys) → body limit.
// An unknown route gets 404 not_found as a problem body.
func (rt *Router) Handler(serviceKeys []string) http.Handler {
	routes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	return RequestID(BFF(serviceKeys)(limitBody(routes)))
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
