package httpx

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/netip"
	"strings"
)

// The audiences a BFF can declare in X-MHAI-Audience.
const (
	AudienceClinical = "clinical"
	AudiencePlatform = "platform"
)

// BFF admits only requests carrying one of keys in X-MHAI-Service, and
// rejects the rest with 401 unauthenticated. Only after the key passes are
// the trusted headers X-MHAI-Client-IP and X-MHAI-Audience read into the
// context (ClientIP, Audience).
//
// Keys are compared as SHA-256 digests with subtle.ConstantTimeCompare,
// against every configured key without stopping early, so the time taken
// reveals neither the key nor its length. Empty keys are ignored; with no
// keys every request is rejected.
func BFF(keys []string) func(http.Handler) http.Handler {
	var digests [][sha256.Size]byte
	for _, k := range keys {
		if k != "" {
			digests = append(digests, sha256.Sum256([]byte(k)))
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !validKey(digests, r.Header.Values("X-MHAI-Service")) {
				Error(w, r, "unauthenticated")
				return
			}
			ctx := r.Context()
			if ip, ok := parseClientIP(r.Header.Values("X-MHAI-Client-IP")); ok {
				ctx = context.WithValue(ctx, clientIPKey, ip)
			}
			aud := AudienceClinical
			if v := r.Header.Values("X-MHAI-Audience"); len(v) == 1 && v[0] == AudiencePlatform {
				aud = AudiencePlatform
			}
			ctx = context.WithValue(ctx, audienceKey, aud)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func validKey(digests [][sha256.Size]byte, got []string) bool {
	if len(got) != 1 || got[0] == "" {
		return false
	}
	d := sha256.Sum256([]byte(got[0]))
	match := 0
	for i := range digests {
		match |= subtle.ConstantTimeCompare(d[:], digests[i][:])
	}
	return match == 1
}

// parseClientIP accepts exactly one header holding one address. Anything
// else (a list, a name, garbage) is treated as missing.
func parseClientIP(vals []string) (netip.Addr, bool) {
	if len(vals) != 1 {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(vals[0]))
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap().WithZone(""), true
}

// ClientIP is the end user's address as reported by the BFF, or the zero
// (invalid) Addr when it is missing, malformed or the request didn't pass
// BFF.
func ClientIP(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(clientIPKey).(netip.Addr)
	return ip
}

// Audience is "platform" when a BFF-authenticated request declared it, and
// "clinical" otherwise.
func Audience(ctx context.Context) string {
	if a, ok := ctx.Value(audienceKey).(string); ok {
		return a
	}
	return AudienceClinical
}
