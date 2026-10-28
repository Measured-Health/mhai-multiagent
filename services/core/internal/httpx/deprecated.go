package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Deprecated marks a route as deprecated for a retirement under ADR 0012.
// The returned middleware sets, on every response the route gives
// (errors included):
//
//	Deprecation: @<unix seconds of deprecated>   (RFC 9745)
//	Sunset: <HTTP-date of sunset>                (RFC 8594)
//	Link: <link>; rel="deprecation"; type="text/html"
//
// link is an absolute http(s) URL or an absolute path to the notice that
// tells clients what to move to. The values are fixed at construction, so a
// bad one (sunset not after deprecated, an unusable link) panics at
// start-up and in every test that builds the route.
func Deprecated(deprecated, sunset time.Time, link string) func(http.Handler) http.Handler {
	if !sunset.After(deprecated) {
		panic("httpx: Deprecated needs a sunset after the deprecation time")
	}
	if link == "" || strings.ContainsAny(link, "<>\r\n\" \t") ||
		!(strings.HasPrefix(link, "/") || strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "http://")) {
		panic("httpx: Deprecated needs an absolute URL or path for link")
	}
	dep := "@" + strconv.FormatInt(deprecated.Unix(), 10)
	sun := sunset.UTC().Format(http.TimeFormat)
	lnk := "<" + link + `>; rel="deprecation"; type="text/html"`
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Deprecation", dep)
			h.Set("Sunset", sun)
			h.Add("Link", lnk)
			next.ServeHTTP(w, r)
		})
	}
}
