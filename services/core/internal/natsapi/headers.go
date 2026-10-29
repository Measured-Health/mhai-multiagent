package natsapi

import (
	"net/http"
	"net/textproto"
	"strings"

	"github.com/nats-io/nats.go"
)

// The adapter's own headers (spec §17.1).
const (
	HeaderMethod  = "Mhai-Method"      // the HTTP method
	HeaderPath    = "Mhai-Path"        // path and query, under /api/v1/
	HeaderTimeout = "Mhai-Timeout-Ms"  // the BFF's deadline, at most MaxTimeout
	HeaderStatus  = "Mhai-Status-Code" // on the reply: the HTTP status
)

// RequestHeaders are the only message headers that reach the handler
// chain, as HTTP headers. Everything else is dropped, notably:
//   - X-MHAI-Service: on NATS the BFF's NATS user is the credential;
//   - X-MHAI-Audience: the audience is the subject's (httpx.ViaNATS);
//   - X-Request-ID and Mhai-Request-Id: core makes its own request id, so
//     none can be forged into the audit chain (plan departure 15);
//   - baggage: never propagated (spec §10.3); only traceparent and
//     tracestate pass, and the server span treats them as untrusted;
//   - Cookie, Host, X-Forwarded-*, Forwarded, X-Real-IP, hop-by-hop
//     (Connection, Transfer-Encoding, TE, Upgrade, ...) and Content-Length
//     (set from the body).
var RequestHeaders = []string{
	"Authorization",
	"Content-Type",
	"Idempotency-Key",
	"If-None-Match",
	"X-MHAI-Client-IP",
	"traceparent",
	"tracestate",
}

// ResponseHeaders are the only response headers sent back to the BFF,
// spelled as here. The spec's list (Content-Type, Cache-Control, ETag,
// Retry-After, X-Request-ID) plus the headers the contract documents that
// core sets on some routes: Idempotent-Replayed (an Idempotency-Key
// replay), Deprecation, Sunset and Link (ADR 0012, httpx.Deprecated), and
// X-Content-Type-Options.
var ResponseHeaders = []string{
	"Content-Type",
	"Cache-Control",
	"ETag",
	"Retry-After",
	"X-Request-ID",
	"Idempotent-Replayed",
	"Deprecation",
	"Sunset",
	"Link",
	"X-Content-Type-Options",
}

var requestAllowed = canonicalSet(RequestHeaders)

func canonicalSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[textproto.CanonicalMIMEHeaderKey(n)] = true
	}
	return m
}

// copyRequestHeaders copies the allow-listed headers of a NATS message
// (whose keys are case-sensitive) into h, matching names in any case.
func copyRequestHeaders(h http.Header, in nats.Header) {
	for k, vs := range in {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if !requestAllowed[ck] {
			continue
		}
		for _, v := range vs {
			h.Add(ck, v)
		}
	}
}

// single returns the only value of the header name in any case, and false
// when it is missing or given more than once.
func single(in nats.Header, name string) (string, bool) {
	var vals []string
	for k, vs := range in {
		if strings.EqualFold(k, name) {
			vals = append(vals, vs...)
		}
	}
	if len(vals) != 1 {
		return "", false
	}
	return vals[0], true
}

// replyHeaders copies the allow-listed response headers into a NATS
// header, under the spelling in ResponseHeaders.
func replyHeaders(out nats.Header, h http.Header) {
	for _, name := range ResponseHeaders {
		if vs := h.Values(name); len(vs) > 0 {
			out[name] = append([]string(nil), vs...)
		}
	}
}
