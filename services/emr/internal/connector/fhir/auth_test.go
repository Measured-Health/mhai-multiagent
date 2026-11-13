package fhir

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

func decodeSegment(t *testing.T, s string) map[string]any {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The client assertion (design §5.1): RS384 for an RSA key, ES384 for a
// P-384 key, anything else sign_in_refused with nothing sent; iss and sub
// the client id, aud the token address, exp 5 minutes, a fresh jti.
func TestAssertionClaimsAndAlg(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	rsaPEM, rsaPub := rsaKey(t)
	ecPEM, ecPub := ecKey(t, elliptic.P384())
	for name, c := range map[string]struct {
		pem []byte
		pub any
		alg string
	}{"rsa": {rsaPEM, rsaPub, "RS384"}, "p-384": {ecPEM, ecPub, "ES384"}} {
		f := newFake(t, c.pub)
		jtis := map[string]bool{}
		for i := 0; i < 2; i++ {
			tok, exp, err := Token(context.Background(), f.srv.Client(), f.cfg(), credsWith(c.pem), now)
			if err != nil || tok == "" || !exp.Equal(now.Add(300*time.Second)) {
				t.Fatalf("%s: token %q %v %v", name, tok, exp, err)
			}
		}
		if len(f.assertions) != 2 {
			t.Fatalf("%s: %d assertions", name, len(f.assertions))
		}
		for _, a := range f.assertions {
			parts := strings.Split(a, ".")
			h, cl := decodeSegment(t, parts[0]), decodeSegment(t, parts[1])
			if h["alg"] != c.alg || h["typ"] != "JWT" || h["kid"] == "" || h["kid"] == nil {
				t.Errorf("%s: header %v", name, h)
			}
			if cl["iss"] != clientID || cl["sub"] != clientID || cl["aud"] != f.tokenURL() ||
				cl["exp"] != float64(now.Add(5*time.Minute).Unix()) || cl["jti"] == nil {
				t.Errorf("%s: claims %v", name, cl)
			}
			jti, _ := cl["jti"].(string)
			if len(jti) < 22 || jtis[jti] {
				t.Errorf("%s: jti %q not fresh", name, jti)
			}
			jtis[jti] = true
		}
	}
	// Other key types and junk: refused before any request.
	p256, _ := ecKey(t, elliptic.P256())
	f := newFake(t, rsaPub)
	for name, k := range map[string][]byte{"p-256": p256, "ed25519": edKey(t), "junk": []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"),
		"empty": nil} {
		_, _, err := Token(context.Background(), f.srv.Client(), f.cfg(), credsWith(k), now)
		var fe *connector.FetchError
		if !errors.As(err, &fe) || fe.Code != "sign_in_refused" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := len(f.reqs()); n != 0 {
		t.Fatalf("%d requests sent with an unusable key", n)
	}
	// A server that refuses the client: sign_in_refused.
	other, _ := rsaKey(t)
	if _, _, err := Token(context.Background(), f.srv.Client(), f.cfg(), credsWith(other), now); connector.CodeOf(err) != "sign_in_refused" {
		t.Fatalf("refused client: %v", err)
	}
}

// A28 (part): the assertion and the token go only to the credential
// origin. A token address on a host outside it gets nothing, and neither
// does the FHIR server.
func TestTokenOnlyToTokenOrigin(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	elsewhere := newFake(t, pub)
	cfg := f.cfg()
	cfg.Settings["token_url"] = elsewhere.tokenURL()
	c := New(func(connector.Config) *http.Client { return f.srv.Client() })
	if r := c.Test(context.Background(), cfg, credsWith(key)); r.Code != "sign_in_refused" {
		t.Fatalf("test with the token address off the origin: %+v", r)
	}
	if _, err := c.Fetch(context.Background(), cfg, credsWith(key), nil, &memSink{}); connector.CodeOf(err) != "sign_in_refused" {
		t.Fatalf("fetch with the token address off the origin: %v", err)
	}
	if len(elsewhere.reqs()) != 0 || len(f.reqs()) != 0 {
		t.Fatalf("requests sent: token host %v, server %v", elsewhere.reqs(), f.reqs())
	}
	// Both on the origin, but the token address over http: refused too.
	cfg.Settings["token_url"] = strings.Replace(f.tokenURL(), "https://", "http://", 1)
	if r := c.Test(context.Background(), cfg, credsWith(key)); r.Code != "sign_in_refused" || len(f.reqs()) != 0 {
		t.Fatalf("http token address: %+v %v", r, f.reqs())
	}
	// The origin names the token host: accepted.
	cfg = f.cfg()
	cfg.Settings["token_url"] = elsewhere.tokenURL()
	u, _ := url.Parse(elsewhere.srv.URL)
	cfg.Origin = append(cfg.Origin, emrurl.Origin("https://"+u.Host))
	if _, _, err := Token(context.Background(), elsewhere.srv.Client(), cfg, credsWith(key), time.Now()); err != nil {
		t.Fatalf("token on a listed origin: %v", err)
	}
}

// The capability checks (design §5.1, §5.2): R4 only, every resource read,
// with _lastUpdated and _sort declared; anything else unreadable_data.
func TestCapabilityChecks(t *testing.T) {
	key, pub := rsaKey(t)
	full := []string{"_lastUpdated", "_sort", "_id"}
	all := map[string][]string{"Patient": full, "Observation": full, "MedicationRequest": full, "MedicationStatement": full,
		"Encounter": full, "Location": full}
	without := func(ty string, params []string) map[string][]string {
		m := map[string][]string{}
		for k, v := range all {
			m[k] = v
		}
		if params == nil {
			delete(m, ty)
		} else {
			m[ty] = params
		}
		return m
	}
	for name, c := range map[string]struct {
		cap  map[string]any
		want string
	}{
		"r4":               {capability("4.0.1", nil), "ok"},
		"r5":               {capability("5.0.0", nil), "unreadable_data"},
		"r4b":              {capability("4.3.0", nil), "unreadable_data"},
		"no version":       {capability("", nil), "unreadable_data"},
		"missing sort":     {capability("4.0.1", without("Observation", []string{"_lastUpdated", "_id"})), "unreadable_data"},
		"missing lu":       {capability("4.0.1", without("MedicationStatement", []string{"_sort"})), "unreadable_data"},
		"missing location": {capability("4.0.1", without("Location", nil)), "unreadable_data"},
		"not a capability": {map[string]any{"resourceType": "Bundle", "fhirVersion": "4.0.1"}, "unreadable_data"},
	} {
		f := newFake(t, pub)
		f.capability = c.cap
		conn := New(func(connector.Config) *http.Client { return f.srv.Client() })
		if r := conn.Test(context.Background(), f.cfg(), credsWith(key)); r.Code != c.want || r.OK != (c.want == "ok") {
			t.Errorf("%s: %+v, want %s", name, r, c.want)
		}
		if c.want == "ok" {
			got := strings.Join(f.reqs(), "\n")
			if !strings.Contains(got, "POST /token") || !strings.Contains(got, "GET /r4/metadata") ||
				!strings.Contains(got, "GET /r4/Observation?_summary=count") || len(f.reqs()) != 3 {
				t.Errorf("ok test sent %v", f.reqs())
			}
		}
	}
}

// A1, the emr side: nothing a full test or fetch does logs the token, the
// assertion or the key.
func TestTokenNeverLogged(t *testing.T) {
	key, pub := rsaKey(t)
	f := newFake(t, pub)
	f.add("Observation", obsRes("o-1", "2026-11-10T08:00:00Z", "p-1"))
	f.add("Patient", patientRes("p-1", "2026-11-10T07:00:00Z"))
	var buf bytes.Buffer
	prevSlog, prevLog := slog.Default(), log.Writer()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.SetOutput(&buf)
	defer func() { slog.SetDefault(prevSlog); log.SetOutput(prevLog) }()
	c := New(func(connector.Config) *http.Client { return f.srv.Client() })
	c.Now = func() time.Time { return time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC) }
	if r := c.Test(context.Background(), f.cfg(), credsWith(key)); !r.OK {
		t.Fatalf("test: %+v", r)
	}
	if _, err := c.Fetch(context.Background(), f.cfg(), credsWith(key), nil, &memSink{}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	out := buf.String()
	secrets := append([]string{"tok-", "BEGIN", strings.Split(string(key), "\n")[1][:20]}, f.assertions...)
	for _, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Fatalf("log holds a secret: %q", out)
		}
	}
}
