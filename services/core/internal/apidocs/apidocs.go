// Package apidocs serves the browsable API documentation: a Redoc page, its
// vendored bundle and the OpenAPI file, all embedded in the binary.
//
// These are plain HTTP routes outside the authenticated API chain (like the
// health probes), so they need no BFF key or token, and they are not in the
// permission table or the OpenAPI contract. They are never served over NATS:
// the NATS adapter wraps the authz.Mux handler, which does not include this
// package. Wrap is applied only around the HTTP listener (cmd/core/serve.go).
package apidocs

import (
	_ "embed"
	"net/http"
	"strconv"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
)

// Paths served.
const (
	PagePath   = "/api/docs"
	BundlePath = "/api/docs/redoc.standalone.js"
	SpecPath   = "/api/openapi.yaml"
)

//go:embed assets/redoc.standalone.js
var bundle []byte

// page is static; the spec is loaded from SpecPath by the bundle.
// disable-search: Redoc's search needs a blob: web worker.
const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>MHAI core API</title>
</head>
<body>
<redoc spec-url="` + SpecPath + `" disable-search></redoc>
<script src="` + BundlePath + `"></script>
</body>
</html>
`

// CSP allows only same-origin scripts, connections and images. Redoc's
// styled-components writes <style> elements and style attributes at run
// time, which a nonce cannot cover, so style-src needs 'unsafe-inline';
// scripts stay 'self' only, so injected markup cannot run code.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'none'"

type asset struct {
	contentType string
	body        []byte
}

// Wrap serves the docs routes ahead of next. When enabled is false the same
// paths answer the normal 404 not_found problem instead of reaching next
// (which would otherwise answer 401 to a request without a BFF key). Every
// other path goes to next unchanged. spec is the OpenAPI file's bytes.
func Wrap(enabled bool, spec []byte, next http.Handler) http.Handler {
	assets := map[string]asset{
		PagePath:   {"text/html; charset=utf-8", []byte(page)},
		BundlePath: {"text/javascript; charset=utf-8", bundle},
		SpecPath:   {"application/yaml; charset=utf-8", spec},
	}
	notFound := httpx.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, r, "not_found")
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != SpecPath && p != PagePath && (len(p) < len(PagePath)+1 || p[:len(PagePath)+1] != PagePath+"/") {
			next.ServeHTTP(w, r)
			return
		}
		// Only exact keys are served: no file system, so "..", encoded
		// slashes, a trailing slash or any other path is a 404.
		a, ok := assets[p]
		if !enabled || !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			notFound.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", a.contentType)
		h.Set("Content-Length", strconv.Itoa(len(a.body)))
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", CSP)
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(a.body)
		}
	})
}
