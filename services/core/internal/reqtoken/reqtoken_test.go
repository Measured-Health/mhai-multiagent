package reqtoken

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// Synthetic ids only.
const (
	testUser     = "11111111-1111-4111-8111-111111111111"
	testHospital = "22222222-2222-4222-8222-222222222222"
	otherHosp    = "33333333-3333-4333-8333-333333333333"
	testRid      = "44444444-4444-4444-8444-444444444444"
	testPatient  = "55555555-5555-4555-8555-555555555555"
)

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func newSigner(t *testing.T) Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return Signer{Key: priv, KeyID: KeyID(priv.Public().(ed25519.PublicKey)), Now: func() time.Time { return testNow }}
}

func reqClaims() Claims {
	return Claims{Aud: "echo", Sub: testUser, Hid: testHospital, Roles: []string{"clinician"}, Rid: testRid,
		Kind: KindReq, Subject: "inference.v1.echo.ping", PatientIDs: []string{testPatient},
		ReplyPrefix: ReplyCore, Via: ViaCore}
}

func jobClaims() Claims {
	return Claims{Aud: "pipeline", Sub: testUser, Hid: testHospital, Roles: []string{"clinician"}, Rid: testRid,
		Kind: KindJob, Subject: "jobs.v1." + testHospital + ".run_query", Via: ViaCore}
}

func reqExpect() Expect { return Expect{Aud: "echo", Subject: "inference.v1.echo.ping", Kind: KindReq} }

func mint(t *testing.T, s Signer, c Claims, body []byte, ttl time.Duration) string {
	t.Helper()
	tok, _, err := s.Mint(c, body, ttl)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

// resign decodes tok, lets edit change the header and payload, and signs
// the result with s's key (a token core never minted, but validly signed).
func resign(t *testing.T, s Signer, tok string, edit func(h, p map[string]any)) string {
	t.Helper()
	parts := strings.Split(tok, ".")
	var h, p map[string]any
	for i, dst := range []*map[string]any{&h, &p} {
		raw, err := b64.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(dst); err != nil {
			t.Fatal(err)
		}
	}
	edit(h, p)
	return signRaw(t, s.Key, h, p)
}

func signRaw(t *testing.T, key ed25519.PrivateKey, h, p any) string {
	t.Helper()
	hj, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	pj, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	in := b64.EncodeToString(hj) + "." + b64.EncodeToString(pj)
	return in + "." + b64.EncodeToString(ed25519.Sign(key, []byte(in)))
}

// signRawBytes signs exact header and payload bytes.
func signRawBytes(key ed25519.PrivateKey, h, p string) string {
	in := b64.EncodeToString([]byte(h)) + "." + b64.EncodeToString([]byte(p))
	return in + "." + b64.EncodeToString(ed25519.Sign(key, []byte(in)))
}

func decodeSeg(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := b64.DecodeString(seg)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMintVerifyRoundTrip(t *testing.T) {
	s := newSigner(t)
	body := []byte("synthetic body")
	for _, tc := range []struct {
		name string
		c    Claims
		want Expect
		ttl  time.Duration
	}{
		{"req", reqClaims(), reqExpect(), 5 * time.Second},
		{"job", jobClaims(), Expect{Aud: "pipeline", Subject: "jobs.v1." + testHospital + ".run_query", Kind: KindJob}, 10 * time.Minute},
		{"global job", Claims{Aud: "pipeline", Sub: SvcCore, Rid: testRid, Kind: KindJob,
			Subject: "jobs.v1.global.retention_purge", Via: ViaCore},
			Expect{Aud: "pipeline", Subject: "jobs.v1.global.retention_purge", Kind: KindJob}, time.Hour},
		{"grant", Claims{Aud: "echo", Sub: testUser, Hid: testHospital, Roles: []string{"clinician"}, Rid: testRid,
			Kind: KindReq, Subject: "inference.v1.echo.ping", ReplyPrefix: "_INBOX_bff.clinical.", Via: ViaGrant},
			reqExpect(), 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, minted, err := s.Mint(tc.c, body, tc.ttl)
			if err != nil {
				t.Fatal(err)
			}
			got, err := VerifyAt(tok, s.PublicKeys(), tc.want, body, testNow.Add(time.Second))
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			want := tc.c
			if got.Aud != want.Aud || got.Sub != want.Sub || got.Hid != want.Hid || got.Rid != want.Rid ||
				got.Kind != want.Kind || got.Subject != want.Subject || got.ReplyPrefix != want.ReplyPrefix ||
				got.Via != want.Via || strings.Join(got.Roles, ",") != strings.Join(want.Roles, ",") ||
				strings.Join(got.PatientIDs, ",") != strings.Join(want.PatientIDs, ",") {
				t.Fatalf("claims differ:\n got %+v\nwant %+v", got, want)
			}
			if got.Jti == "" || got.Jti != minted.Jti || got.KeyID != s.KeyID {
				t.Fatalf("jti %q minted %q kid %q", got.Jti, minted.Jti, got.KeyID)
			}
			if raw, err := b64.DecodeString(got.Jti); err != nil || len(raw) != 16 {
				t.Fatalf("jti is not 128 bits of base64url: %q", got.Jti)
			}
			sum := sha256.Sum256(body)
			if got.BodyHash != b64.EncodeToString(sum[:]) || minted.BodyHash != got.BodyHash {
				t.Fatalf("bh %q", got.BodyHash)
			}
			if !got.IssuedAt.Equal(testNow) || got.ExpiresAt.Sub(got.IssuedAt) != tc.ttl ||
				!minted.ExpiresAt.Equal(got.ExpiresAt) {
				t.Fatalf("iat %v exp %v", got.IssuedAt, got.ExpiresAt)
			}
			parts := strings.Split(tok, ".")
			h, p := decodeSeg(t, parts[0]), decodeSeg(t, parts[1])
			if h["typ"] != "mhai-req+jwt" || h["alg"] != "EdDSA" || h["kid"] != s.KeyID || len(h) != 3 {
				t.Fatalf("header %v", h)
			}
			if p["iss"] != "mhai-gateway" || p["exp"].(float64)-p["iat"].(float64) != tc.ttl.Seconds() {
				t.Fatalf("payload %v", p)
			}
			if _, ok := p["pids"]; ok != (len(tc.c.PatientIDs) > 0) {
				t.Fatalf("pids present=%v, want omitted when empty", ok)
			}
			if _, ok := p["rpl"]; ok != (tc.c.Kind == KindReq) {
				t.Fatalf("rpl present=%v on %s", ok, tc.c.Kind)
			}
		})
	}
	// Two mints never share a jti.
	a, _, _ := s.Mint(reqClaims(), body, time.Second)
	b, _, _ := s.Mint(reqClaims(), body, time.Second)
	ca, _ := VerifyAt(a, s.PublicKeys(), reqExpect(), body, testNow)
	cb, _ := VerifyAt(b, s.PublicKeys(), reqExpect(), body, testNow)
	if ca.Jti == "" || ca.Jti == cb.Jti {
		t.Fatalf("jti reused: %q %q", ca.Jti, cb.Jti)
	}
}

// A6: an access token, even signed with a trusted request key, is not a
// request token (its typ is JWT).
func TestAccessTokenRefusedAsRequestToken(t *testing.T) {
	s := newSigner(t)
	iss := token.Issuer{Key: s.Key, KeyID: s.KeyID, Now: func() time.Time { return testNow }}
	acc, _, err := iss.Access(token.Claims{Sub: testUser, Hid: testHospital, Roles: []string{"clinician"},
		Sid: testRid, Jti: testPatient})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAt(acc, s.PublicKeys(), reqExpect(), nil, testNow); !errors.Is(err, ErrSignature) {
		t.Fatalf("access token as request token: %v, want ErrSignature", err)
	}
}

// A6, reverse: a request token is not an access token.
func TestRequestTokenRefusedAsAccessToken(t *testing.T) {
	s := newSigner(t)
	tok := mint(t, s, reqClaims(), nil, 5*time.Second)
	if _, err := token.VerifyAt(tok, s.PublicKeys(), testNow); err == nil {
		t.Fatal("token.VerifyAt accepted a request token")
	}
}

// A7: the token is bound to the body bytes and the subject.
func TestBodyHashBinding(t *testing.T) {
	s := newSigner(t)
	body := []byte("synthetic body bytes")
	tok := mint(t, s, reqClaims(), body, 5*time.Second)
	if _, err := VerifyAt(tok, s.PublicKeys(), reqExpect(), body, testNow); err != nil {
		t.Fatal(err)
	}
	for i := range body {
		flipped := append([]byte(nil), body...)
		flipped[i] ^= 1
		if _, err := VerifyAt(tok, s.PublicKeys(), reqExpect(), flipped, testNow); !errors.Is(err, ErrBinding) {
			t.Fatalf("byte %d flipped: %v, want ErrBinding", i, err)
		}
	}
	if _, err := VerifyAt(tok, s.PublicKeys(), reqExpect(), append(body, 'x'), testNow); !errors.Is(err, ErrBinding) {
		t.Fatalf("appended byte: %v", err)
	}
	other := reqExpect()
	other.Subject = "inference.v1.echo.other"
	if _, err := VerifyAt(tok, s.PublicKeys(), other, body, testNow); !errors.Is(err, ErrBinding) {
		t.Fatalf("other subject: %v, want ErrBinding", err)
	}
	asJob := reqExpect()
	asJob.Kind = KindJob
	if _, err := VerifyAt(tok, s.PublicKeys(), asJob, body, testNow); !errors.Is(err, ErrBinding) {
		t.Fatalf("req token taken as a job: %v, want ErrBinding", err)
	}
	// A job's subject hospital must equal hid.
	job := mint(t, s, jobClaims(), body, time.Minute)
	forged := resign(t, s, job, func(_, p map[string]any) { p["nsub"] = "jobs.v1." + otherHosp + ".run_query" })
	want := Expect{Aud: "pipeline", Subject: "jobs.v1." + otherHosp + ".run_query", Kind: KindJob}
	if _, err := VerifyAt(forged, s.PublicKeys(), want, body, testNow); !errors.Is(err, ErrBinding) {
		t.Fatalf("job for another hospital's subject: %v, want ErrBinding", err)
	}
}

// A10: alg, kid and lifetime.
func TestRequestTokenRejectsAlgAndKidAndLifetime(t *testing.T) {
	s := newSigner(t)
	pub := s.Key.Public().(ed25519.PublicKey)
	body := []byte("b")
	good := mint(t, s, reqClaims(), body, 5*time.Second)
	parts := strings.Split(good, ".")
	hdr := func(alg string) string {
		j, _ := json.Marshal(map[string]string{"alg": alg, "typ": Type, "kid": s.KeyID})
		return b64.EncodeToString(j)
	}
	hsIn := hdr("HS256") + "." + parts[1]
	mac := hmac.New(sha256.New, pub)
	mac.Write([]byte(hsIn))
	job := mint(t, s, jobClaims(), body, time.Minute)
	jobWant := Expect{Aud: "pipeline", Subject: jobClaims().Subject, Kind: KindJob}

	cases := []struct {
		name string
		tok  string
		want Expect
		now  time.Time
		err  error
	}{
		{"alg none", hdr("none") + "." + parts[1] + ".", reqExpect(), testNow, ErrSignature},
		{"alg none with junk signature", hdr("none") + "." + parts[1] + "." + parts[2], reqExpect(), testNow, ErrSignature},
		{"alg HS256", hsIn + "." + b64.EncodeToString(mac.Sum(nil)), reqExpect(), testNow, ErrSignature},
		{"unknown kid", resign(t, s, good, func(h, _ map[string]any) { h["kid"] = "0000000000000000" }), reqExpect(), testNow, ErrSignature},
		{"job exp 25h after iat", resign(t, s, job, func(_, p map[string]any) {
			p["exp"] = testNow.Add(25 * time.Hour).Unix()
		}), jobWant, testNow, ErrClaims},
		{"req exp 61s after iat", resign(t, s, good, func(_, p map[string]any) {
			p["exp"] = testNow.Add(61 * time.Second).Unix()
		}), reqExpect(), testNow, ErrClaims},
		{"iat 6s in the future", resign(t, s, good, func(_, p map[string]any) {
			p["iat"] = testNow.Add(6 * time.Second).Unix()
			p["exp"] = testNow.Add(11 * time.Second).Unix()
		}), reqExpect(), testNow, ErrClaims},
		{"iat and exp overflow", resign(t, s, good, func(_, p map[string]any) {
			p["iat"] = int64(-9e18)
			p["exp"] = int64(9e18)
		}), reqExpect(), testNow, ErrClaims},
		{"iat zero", resign(t, s, good, func(_, p map[string]any) { p["iat"], p["exp"] = 0, 1 }), reqExpect(), time.Unix(0, 0), ErrClaims},
		{"exp over 2^53", resign(t, s, good, func(_, p map[string]any) {
			p["iat"] = int64(1<<53) - 1
			p["exp"] = int64(1<<53) + 1
		}), reqExpect(), testNow, ErrClaims},
		{"exp before iat", resign(t, s, good, func(_, p map[string]any) {
			p["exp"] = testNow.Add(-time.Second).Unix()
		}), reqExpect(), testNow.Add(-2 * time.Second), ErrClaims},
		{"expired", good, reqExpect(), testNow.Add(5 * time.Second), ErrExpired},
		{"wrong iss", resign(t, s, good, func(_, p map[string]any) { p["iss"] = "mhai-core" }), reqExpect(), testNow, ErrClaims},
		{"wrong aud", good, Expect{Aud: "pipeline", Subject: "inference.v1.echo.ping", Kind: KindReq}, testNow, ErrClaims},
		{"missing rid", resign(t, s, good, func(_, p map[string]any) { delete(p, "rid") }), reqExpect(), testNow, ErrClaims},
		{"empty rid", resign(t, s, good, func(_, p map[string]any) { p["rid"] = "" }), reqExpect(), testNow, ErrClaims},
		{"missing hid on a hospital subject", resign(t, s, good, func(_, p map[string]any) { p["hid"] = "" }), reqExpect(), testNow, ErrClaims},
		{"unknown knd", resign(t, s, good, func(_, p map[string]any) { p["knd"] = "stream" }), reqExpect(), testNow, ErrClaims},
		{"unknown via", resign(t, s, good, func(_, p map[string]any) { p["via"] = "bff" }), reqExpect(), testNow, ErrClaims},
		{"req without rpl", resign(t, s, good, func(_, p map[string]any) { delete(p, "rpl") }), reqExpect(), testNow, ErrClaims},
		{"job with rpl", resign(t, s, job, func(_, p map[string]any) { p["rpl"] = ReplyCore }), jobWant, testNow, ErrClaims},
		{"job via grant", resign(t, s, job, func(_, p map[string]any) { p["via"] = "grant" }), jobWant, testNow, ErrClaims},
		{"tampered payload", parts[0] + "." + resignedPayload(t, s, good) + "." + parts[2], reqExpect(), testNow, ErrSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifyAt(tc.tok, s.PublicKeys(), tc.want, body, tc.now)
			if !errors.Is(err, tc.err) {
				t.Fatalf("got %v, want %v", err, tc.err)
			}
			if err != nil && strings.Contains(err.Error(), tc.tok) {
				t.Fatal("error holds the token")
			}
		})
	}
	// The lifetime caps themselves are accepted.
	atCap := mint(t, s, jobClaims(), body, MaxJobTTL)
	if _, err := VerifyAt(atCap, s.PublicKeys(), jobWant, body, testNow.Add(23*time.Hour)); err != nil {
		t.Fatalf("24h job: %v", err)
	}
	// iat 5s in the future is within the skew.
	skew := resign(t, s, good, func(_, p map[string]any) {
		p["iat"] = testNow.Add(Skew).Unix()
		p["exp"] = testNow.Add(Skew + 5*time.Second).Unix()
	})
	if _, err := VerifyAt(skew, s.PublicKeys(), reqExpect(), body, testNow); err != nil {
		t.Fatalf("iat at the skew: %v", err)
	}
}

// resignedPayload returns good's payload segment with roles changed (and
// not re-signed).
func resignedPayload(t *testing.T, s Signer, good string) string {
	t.Helper()
	forged := resign(t, s, good, func(_, p map[string]any) { p["roles"] = []string{"platform_admin"} })
	return strings.Split(forged, ".")[1]
}

func TestMintRefusesOverCap(t *testing.T) {
	s := newSigner(t)
	for _, tc := range []struct {
		c   Claims
		ttl time.Duration
		ok  bool
	}{
		{reqClaims(), MaxReqTTL, true},
		{reqClaims(), MaxReqTTL + time.Second, false},
		{reqClaims(), MaxReqTTL + time.Millisecond, false},
		{jobClaims(), MaxJobTTL, true},
		{jobClaims(), MaxJobTTL + time.Second, false},
		{reqClaims(), 0, false},
		{reqClaims(), 999 * time.Millisecond, false},
		{reqClaims(), -time.Second, false},
	} {
		_, _, err := s.Mint(tc.c, nil, tc.ttl)
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrMintTTL)) {
			t.Errorf("%s ttl %v: %v", tc.c.Kind, tc.ttl, err)
		}
	}
	// A ttl that isn't whole seconds is rounded down, never up.
	_, c, err := s.Mint(reqClaims(), nil, 2500*time.Millisecond)
	if err != nil || c.ExpiresAt.Sub(c.IssuedAt) != 2*time.Second {
		t.Fatalf("2.5s: %v %v", err, c.ExpiresAt.Sub(c.IssuedAt))
	}
	unknown := reqClaims()
	unknown.Kind = "stream"
	if _, _, err := s.Mint(unknown, nil, time.Second); err == nil {
		t.Fatal("minted an unknown kind")
	}
}

// F10: every token carries a hospital except global jobs.
func TestMintRefusesMissingHospital(t *testing.T) {
	s := newSigner(t)
	c := reqClaims()
	c.Hid = ""
	if _, _, err := s.Mint(c, nil, time.Second); !errors.Is(err, ErrMintHospital) {
		t.Fatalf("req without hid: %v", err)
	}
	j := jobClaims()
	j.Hid = ""
	if _, _, err := s.Mint(j, nil, time.Second); !errors.Is(err, ErrMintHospital) {
		t.Fatalf("hospital job without hid: %v", err)
	}
	g := Claims{Aud: "pipeline", Sub: SvcCore, Rid: testRid, Kind: KindJob, Subject: "jobs.v1.global.retention_purge", Via: ViaCore}
	if _, _, err := s.Mint(g, nil, time.Minute); err != nil {
		t.Fatalf("global job: %v", err)
	}
}

func TestMintRefusesBadClaims(t *testing.T) {
	s := newSigner(t)
	for name, edit := range map[string]func(*Claims){
		"no aud":                 func(c *Claims) { c.Aud = "" },
		"no sub":                 func(c *Claims) { c.Sub = "" },
		"no rid":                 func(c *Claims) { c.Rid = "" },
		"no subject":             func(c *Claims) { c.Subject = "" },
		"no via":                 func(c *Claims) { c.Via = "" },
		"aud is not the service": func(c *Claims) { c.Aud = "pipeline" },
		"subject not inference":  func(c *Claims) { c.Subject = "api.v1.clinical" },
		"subject wildcard":       func(c *Claims) { c.Subject = "inference.v1.echo.>" },
		"subject extra level":    func(c *Claims) { c.Subject = "inference.v1.echo.ping.x" },
		"no rpl on req":          func(c *Claims) { c.ReplyPrefix = "" },
		"core via, bff rpl":      func(c *Claims) { c.ReplyPrefix = "_INBOX_bff.clinical." },
		"grant via, core rpl":    func(c *Claims) { c.Via = ViaGrant },
		"grant rpl not an app":   func(c *Claims) { c.Via, c.ReplyPrefix = ViaGrant, "_INBOX_bff." },
		"grant rpl no dot":       func(c *Claims) { c.Via, c.ReplyPrefix = ViaGrant, "_INBOX_bff.clinical" },
		"hid not a uuid":         func(c *Claims) { c.Hid = "hospital-a" },
		"hid upper case":         func(c *Claims) { c.Hid = "AAAAAAAA-2222-4222-8222-222222222222" },
		"svc:core with roles":    func(c *Claims) { c.Sub = SvcCore },
		"empty role":             func(c *Claims) { c.Roles = []string{"clinician", ""} },
		"empty patient id":       func(c *Claims) { c.PatientIDs = []string{""} },
	} {
		c := reqClaims()
		edit(&c)
		if _, _, err := s.Mint(c, nil, time.Second); !errors.Is(err, ErrMintClaims) {
			t.Errorf("%s: %v, want ErrMintClaims", name, err)
		}
	}
	for name, edit := range map[string]func(*Claims){
		"job with rpl":            func(c *Claims) { c.ReplyPrefix = ReplyCore },
		"job via grant":           func(c *Claims) { c.Via = ViaGrant },
		"job subject other hosp":  func(c *Claims) { c.Subject = "jobs.v1." + otherHosp + ".run_query" },
		"job on inference":        func(c *Claims) { c.Subject = "inference.v1.pipeline.health" },
		"job subject bad type":    func(c *Claims) { c.Subject = "jobs.v1." + testHospital + ".Run-Query" },
		"job subject no type":     func(c *Claims) { c.Subject = "jobs.v1." + testHospital },
		"global with bad subject": func(c *Claims) { c.Subject = "jobs.v1.GLOBAL.run_query" },
	} {
		c := jobClaims()
		edit(&c)
		if _, _, err := s.Mint(c, nil, time.Minute); !errors.Is(err, ErrMintClaims) {
			t.Errorf("%s: %v, want ErrMintClaims", name, err)
		}
	}
	if _, _, err := (Signer{}).Mint(reqClaims(), nil, time.Second); err == nil {
		t.Fatal("minted without a key")
	}
}

// Strict JSON: no unknown, duplicate or case-folded fields, no trailing
// data, exactly three parts.
func TestStrictHeaderAndPayload(t *testing.T) {
	s := newSigner(t)
	body := []byte("b")
	good := mint(t, s, reqClaims(), body, 5*time.Second)
	parts := strings.Split(good, ".")
	hraw, _ := b64.DecodeString(parts[0])
	praw, _ := b64.DecodeString(parts[1])
	h, p := string(hraw), string(praw)
	hdr := `{"alg":"EdDSA","typ":"mhai-req+jwt","kid":"` + s.KeyID + `"}`
	if h != hdr {
		t.Fatalf("header %s", h)
	}
	cases := map[string]string{
		"unknown header field":   signRawBytes(s.Key, strings.TrimSuffix(hdr, "}")+`,"x5u":"https://example.invalid"}`, p),
		"duplicate header field": signRawBytes(s.Key, strings.TrimSuffix(hdr, "}")+`,"alg":"EdDSA"}`, p),
		"case-folded header key": signRawBytes(s.Key, strings.Replace(hdr, `"alg"`, `"Alg"`, 1), p),
		"header missing kid":     signRawBytes(s.Key, `{"alg":"EdDSA","typ":"mhai-req+jwt"}`, p),
		"header trailing data":   signRawBytes(s.Key, hdr+`{}`, p),
		"header not an object":   signRawBytes(s.Key, `["EdDSA"]`, p),
		"unknown payload field":  signRawBytes(s.Key, hdr, strings.TrimSuffix(p, "}")+`,"adm":true}`),
		"duplicate payload key":  signRawBytes(s.Key, hdr, strings.TrimSuffix(p, "}")+`,"hid":"`+otherHosp+`"}`),
		"case-folded claim":      signRawBytes(s.Key, hdr, strings.Replace(p, `"hid"`, `"HID"`, 1)),
		"payload trailing data":  signRawBytes(s.Key, hdr, p+` {}`),
		"payload wrong type":     signRawBytes(s.Key, hdr, strings.Replace(p, `"knd":"req"`, `"knd":7`, 1)),
		"payload float time": signRawBytes(s.Key, hdr, strings.Replace(p, fmt.Sprintf(`"iat":%d`, testNow.Unix()),
			fmt.Sprintf(`"iat":%d.0`, testNow.Unix()), 1)),
		"null claim":            signRawBytes(s.Key, hdr, strings.Replace(p, `"rid":"`+testRid+`"`, `"rid":null`, 1)),
		"null roles":            signRawBytes(s.Key, hdr, strings.Replace(p, `"roles":["clinician"]`, `"roles":null`, 1)),
		"null header field":     signRawBytes(s.Key, strings.Replace(hdr, `"kid":"`+s.KeyID+`"`, `"kid":null`, 1), p),
		"payload invalid utf-8": signRawBytes(s.Key, hdr, strings.Replace(p, `"clinician"`, "\"clin\xffician\"", 1)),
		"iat bool":              signRawBytes(s.Key, hdr, strings.Replace(p, fmt.Sprintf(`"iat":%d`, testNow.Unix()), `"iat":true`, 1)),
		"null in roles":         signRawBytes(s.Key, hdr, strings.Replace(p, `"roles":["clinician"]`, `"roles":[null]`, 1)),
		"empty role":            signRawBytes(s.Key, hdr, strings.Replace(p, `"roles":["clinician"]`, `"roles":["clinician",""]`, 1)),
		"null in pids":          signRawBytes(s.Key, hdr, strings.Replace(p, `"pids":["`+testPatient+`"]`, `"pids":[null]`, 1)),
		"LF in signature":       parts[0] + "." + parts[1] + "." + parts[2][:10] + "\n" + parts[2][10:],
		"CRLF in signature":     parts[0] + "." + parts[1] + "." + parts[2][:10] + "\r\n" + parts[2][10:],
		"LF at end":             good + "\n",
		"space in payload":      parts[0] + "." + parts[1][:5] + " " + parts[1][5:] + "." + parts[2],
		"four parts":            good + "." + parts[2],
		"two parts":             parts[0] + "." + parts[1],
		"one part":              parts[0],
		"empty":                 "",
		"padded base64":         parts[0] + "=." + parts[1] + "." + parts[2],
		"not base64":            "!!." + parts[1] + "." + parts[2],
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyAt(tok, s.PublicKeys(), reqExpect(), body, testNow); !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
	// The exact bytes Mint wrote verify.
	if _, err := VerifyAt(signRawBytes(s.Key, hdr, p), s.PublicKeys(), reqExpect(), body, testNow); err != nil {
		t.Fatal(err)
	}
}

// One size check (F11, F25): over 4 KB is ErrSize, before any parsing; so
// is a body over its kind's limit. Mint refuses the same.
func TestSizes(t *testing.T) {
	s := newSigner(t)
	big := make([]string, 120)
	for i := range big {
		big[i] = testPatient
	}
	c := reqClaims()
	c.PatientIDs = big
	if _, _, err := s.Mint(c, nil, time.Second); !errors.Is(err, ErrSize) {
		t.Fatalf("mint oversize: %v", err)
	}
	good := mint(t, s, reqClaims(), nil, time.Second)
	over := resign(t, s, good, func(_, p map[string]any) { p["pids"] = big })
	if len(over) <= MaxTokenLen {
		t.Fatalf("test token only %d bytes", len(over))
	}
	if _, err := VerifyAt(over, s.PublicKeys(), reqExpect(), nil, testNow); !errors.Is(err, ErrSize) {
		t.Fatalf("verify oversize: %v", err)
	}
	if _, err := VerifyAt(strings.Repeat("a", MaxTokenLen+1), s.PublicKeys(), reqExpect(), nil, testNow); !errors.Is(err, ErrSize) {
		t.Fatalf("junk oversize: %v", err)
	}
	if _, _, err := s.Mint(reqClaims(), make([]byte, MaxReqBody+1), time.Second); !errors.Is(err, ErrSize) {
		t.Fatalf("mint req body: %v", err)
	}
	if _, _, err := s.Mint(jobClaims(), make([]byte, MaxJobBody+1), time.Second); !errors.Is(err, ErrSize) {
		t.Fatalf("mint job body: %v", err)
	}
	if _, err := VerifyAt(good, s.PublicKeys(), reqExpect(), make([]byte, MaxReqBody+1), testNow); !errors.Is(err, ErrSize) {
		t.Fatalf("verify req body: %v", err)
	}
	job := mint(t, s, jobClaims(), make([]byte, MaxJobBody), time.Minute)
	if _, err := VerifyAt(job, s.PublicKeys(), Expect{Aud: "pipeline", Subject: jobClaims().Subject, Kind: KindJob},
		make([]byte, MaxJobBody+1), testNow); !errors.Is(err, ErrSize) {
		t.Fatalf("verify job body: %v", err)
	}
}

// Check 5: the reply subject, and who may present a token.
func TestCheckReply(t *testing.T) {
	s := newSigner(t)
	core := mint(t, s, reqClaims(), nil, time.Second)
	g := reqClaims()
	g.Via, g.ReplyPrefix = ViaGrant, "_INBOX_bff.clinical."
	grant := mint(t, s, g, nil, time.Second)
	cc, err := VerifyAt(core, s.PublicKeys(), reqExpect(), nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	gc, err := VerifyAt(grant, s.PublicKeys(), reqExpect(), nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	jc, err := VerifyAt(mint(t, s, jobClaims(), nil, time.Minute), s.PublicKeys(),
		Expect{Aud: "pipeline", Subject: jobClaims().Subject, Kind: KindJob}, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		c       Claims
		reply   string
		granted bool
		ok      bool
	}{
		{"core token, core inbox", cc, "_INBOX_gateway.abc.1", false, true},
		{"core token, worker inbox", cc, "_INBOX_worker.echo.1", false, false},
		{"core token, bare prefix", cc, "_INBOX_gateway.", false, false},
		{"core token, no reply", cc, "", false, false},
		{"core token, lookalike prefix", cc, "_INBOX_gatewayX.1", false, false},
		{"core token presented as a grant", cc, "_INBOX_gateway.abc.1", true, false},
		{"grant, its inbox", gc, "_INBOX_bff.clinical.x.1", true, true},
		{"grant, other app's inbox", gc, "_INBOX_bff.platform.x.1", true, false},
		{"grant token in the message", gc, "_INBOX_bff.clinical.x.1", false, false},
		{"job token", jc, "_INBOX_gateway.abc.1", false, false},
	} {
		err := CheckReply(tc.c, tc.reply, tc.granted)
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, ErrReplySubject)) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

func TestReasonCodes(t *testing.T) {
	for err, want := range map[error]string{
		nil: "ok", ErrSize: "size", ErrMalformed: "malformed", ErrSignature: "signature", ErrClaims: "claims",
		ErrExpired: "expired", ErrBinding: "binding", ErrReplySubject: "reply_subject",
		errors.New("anything else"): "malformed",
	} {
		if got := Reason(err); got != want {
			t.Errorf("Reason(%v) = %q, want %q", err, got, want)
		}
	}
}

// LoadSigner reads the same PEM PKCS#8 file as token.LoadIssuer, with the
// same key id rule.
func TestLoadSigner(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	path := filepath.Join(t.TempDir(), "request.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	iss, _ := token.LoadIssuer(path)
	if s.KeyID != iss.KeyID || s.KeyID != KeyID(priv.Public().(ed25519.PublicKey)) || !s.Key.Equal(priv) {
		t.Fatalf("kid %q vs %q", s.KeyID, iss.KeyID)
	}
	junk := filepath.Join(t.TempDir(), "junk.pem")
	_ = os.WriteFile(junk, []byte("not a key"), 0o600)
	for _, p := range []string{junk, filepath.Join(t.TempDir(), "missing.pem")} {
		_, err := LoadSigner(p)
		if err == nil || strings.Contains(err.Error(), p) {
			t.Fatalf("%v", err)
		}
	}
}
