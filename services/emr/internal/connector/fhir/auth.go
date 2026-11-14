package fhir

// SMART Backend Services (design §5.1): a client assertion signed with the
// connection's private key (private_key_jwt), RS384 for an RSA key and
// ES384 for a P-384 key, sent to token_url; the access token is held in
// memory per connection until 60 s before it expires, never stored and
// never logged. The assertion and the token go only to the connection's
// credential origin (design §6.3).

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// DefaultScope is asked for when the connection names none.
const DefaultScope = "system/Patient.rs system/Observation.rs system/MedicationRequest.rs system/MedicationStatement.rs " +
	"system/Encounter.rs system/Location.rs"

// The assertion's lifetime, and how early a cached token is renewed.
const (
	assertionLife = 5 * time.Minute
	renewBefore   = 60 * time.Second
	maxTokenLen   = 4096
	maxTokenBody  = 64 << 10
)

type cachedToken struct {
	token string
	exp   time.Time
	key   [32]byte // what it was issued for: client, token address, key
}

func signInRefused() error { return connector.Fail("sign_in_refused", 0) }

// signer reads the PEM private key: RSA (PKCS#1 or PKCS#8) signs RS384, an
// ECDSA P-384 key ES384. Anything else is refused.
func signer(p []byte) (crypto.Signer, string, error) {
	b, _ := pem.Decode(p)
	if b == nil {
		return nil, "", signInRefused()
	}
	var key any
	var err error
	switch b.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(b.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(b.Bytes)
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(b.Bytes)
	default:
		return nil, "", signInRefused()
	}
	if err != nil {
		return nil, "", signInRefused()
	}
	switch k := key.(type) {
	case *rsa.PrivateKey:
		if k.N.BitLen() < 2048 {
			return nil, "", signInRefused()
		}
		return k, "RS384", nil
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P384() {
			return nil, "", signInRefused()
		}
		return k, "ES384", nil
	}
	return nil, "", signInRefused()
}

// thumbprint is the RFC 7638 JWK thumbprint of the public key: the kid.
func thumbprint(pub crypto.PublicKey) string {
	var j string
	switch k := pub.(type) {
	case *rsa.PublicKey:
		e := b64uint(k.E)
		j = `{"e":"` + e + `","kty":"RSA","n":"` + base64.RawURLEncoding.EncodeToString(k.N.Bytes()) + `"}`
	case *ecdsa.PublicKey:
		// The uncompressed point is 0x04 || X || Y, 48 bytes each on P-384
		// (only P-384 keys are accepted, signingKey above).
		b, err := k.Bytes()
		if err != nil || len(b) != 97 {
			break
		}
		x, y := b[1:49], b[49:]
		j = `{"crv":"P-384","kty":"EC","x":"` + base64.RawURLEncoding.EncodeToString(x) + `","y":"` +
			base64.RawURLEncoding.EncodeToString(y) + `"}`
	}
	s := sha256.Sum256([]byte(j))
	return base64.RawURLEncoding.EncodeToString(s[:])
}

func b64uint(e int) string {
	var b []byte
	for e > 0 {
		b = append([]byte{byte(e)}, b...)
		e >>= 8
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// assertion is the signed client assertion.
func assertion(key crypto.Signer, alg, clientID, aud string, now time.Time) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", signInRefused()
	}
	h, _ := json.Marshal(map[string]string{"alg": alg, "typ": "JWT", "kid": thumbprint(key.Public())})
	c, _ := json.Marshal(map[string]any{"iss": clientID, "sub": clientID, "aud": aud, "iat": now.Unix(),
		"exp": now.Add(assertionLife).Unix(), "jti": hex.EncodeToString(jti)})
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	sum := sha512.Sum384([]byte(in))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		s, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA384, sum[:])
		if err != nil {
			return "", signInRefused()
		}
		sig = s
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, sum[:])
		if err != nil {
			return "", signInRefused()
		}
		sig = make([]byte, 96)
		r.FillBytes(sig[:48])
		s.FillBytes(sig[48:])
	default:
		return "", signInRefused()
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// onOrigin reports whether raw is an https URL at one of origins.
func onOrigin(raw string, origins []emrurl.Origin) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if port == "" {
		port = "443"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return slices.Contains(origins, emrurl.Origin("https://"+host+":"+port))
}

// Token signs in: an access token and when it expires. Errors are
// connector.FetchError codes; nothing is sent unless the key is usable and
// the token address is on the credential origin.
func Token(ctx context.Context, client *http.Client, cfg connector.Config, creds connector.Creds, now time.Time) (string, time.Time, error) {
	tokenURL, clientID := cfg.Settings["token_url"], cfg.Settings["client_id"]
	if clientID == "" || !onOrigin(tokenURL, cfg.Origin) {
		return "", time.Time{}, signInRefused()
	}
	key, alg, err := signer(creds.Get("private_key").Bytes())
	if err != nil {
		return "", time.Time{}, err
	}
	a, err := assertion(key, alg, clientID, tokenURL, now)
	if err != nil {
		return "", time.Time{}, err
	}
	scope := cfg.Settings["scope"]
	if scope == "" {
		scope = DefaultScope
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {scope},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"}, "client_assertion": {a}}
	rctx, cancel := pageContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, signInRefused()
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, transportError(ctx, err)
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxTokenBody)); _ = resp.Body.Close() }()
	if fe := statusError(resp, now); fe != nil {
		if resp.StatusCode == http.StatusBadRequest {
			return "", time.Time{}, signInRefused() // OAuth invalid_client, invalid_grant
		}
		return "", time.Time{}, fe
	}
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := egress.DecodeJSON(io.LimitReader(resp.Body, maxTokenBody), &body, 4); err != nil {
		return "", time.Time{}, connector.Fail("unreadable_data", 0)
	}
	if !strings.EqualFold(body.TokenType, "bearer") || !tokenShape(body.AccessToken) {
		return "", time.Time{}, connector.Fail("unreadable_data", 0)
	}
	life := time.Duration(body.ExpiresIn) * time.Second
	if body.ExpiresIn <= 0 || body.ExpiresIn > 24*3600 {
		life = 0 // unknown: used for this run only, never cached
	}
	return body.AccessToken, now.Add(life), nil
}

// tokenShape: printable ASCII without spaces, so it can't break a header.
func tokenShape(t string) bool {
	if t == "" || len(t) > maxTokenLen {
		return false
	}
	for i := 0; i < len(t); i++ {
		if t[i] <= ' ' || t[i] >= 0x7f {
			return false
		}
	}
	return true
}

// cacheKey binds a cached token to what issued it, so a replaced key, a
// new client id or token address never reuses the old token.
func cacheKey(cfg connector.Config, creds connector.Creds) [32]byte {
	h := sha256.New()
	h.Write([]byte(cfg.Settings["client_id"] + "\x00" + cfg.Settings["token_url"] + "\x00" + cfg.Settings["scope"] + "\x00"))
	h.Write(creds.Get("private_key").Bytes())
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// token is the connection's access token, from the cache while it has
// more than a minute left.
func (c *FHIR) token(ctx context.Context, client *http.Client, cfg connector.Config, creds connector.Creds) (string, error) {
	now, key := c.now(), cacheKey(cfg, creds)
	c.mu.Lock()
	if t, ok := c.tokens[cfg.ConnectionID]; ok && t.key == key && now.Before(t.exp.Add(-renewBefore)) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()
	tok, exp, err := Token(ctx, client, cfg, creds, now)
	if err != nil {
		c.forget(cfg)
		return "", err
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[uuid.UUID]cachedToken{}
	}
	if exp.After(now.Add(renewBefore)) {
		c.tokens[cfg.ConnectionID] = cachedToken{token: tok, exp: exp, key: key}
	} else {
		delete(c.tokens, cfg.ConnectionID)
	}
	c.mu.Unlock()
	return tok, nil
}

// forget drops the connection's cached token (after a refusal).
func (c *FHIR) forget(cfg connector.Config) {
	c.mu.Lock()
	delete(c.tokens, cfg.ConnectionID)
	c.mu.Unlock()
}
