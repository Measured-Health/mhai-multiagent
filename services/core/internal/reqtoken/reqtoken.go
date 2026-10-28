// Package reqtoken mints the request tokens core puts on every call and job
// to the intelligence service (spec §7.2, §17.2), and verifies them in Go
// (spec §7.3 checks 1-5) for tests, the shared vectors and core's own fake
// worker. The production verifier is the Python twin.
//
// Like the access token, the JWT is built by hand: one algorithm (EdDSA over
// Ed25519), one header shape, a fixed claim set and strict JSON, so there is
// no algorithm negotiation to get wrong. The typ differs from the access
// token's, so neither verifier accepts the other's token, and the signing
// key is a separate file (REQUEST_KEY_PATH). Errors are fixed values and
// never contain the token, a claim value or a path.
package reqtoken

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// Fixed values (spec §7.2, plan global constraints).
const (
	Type        = "mhai-req+jwt"   // the header typ
	Issuer      = token.IssuerName // iss: mhai-gateway
	SvcCore     = "svc:core"       // sub for work no user started
	MaxReqTTL   = 60 * time.Second
	MaxJobTTL   = 24 * time.Hour
	Skew        = 5 * time.Second
	MaxTokenLen = 4096
	MaxReqBody  = 256 << 10 // request/reply body
	MaxJobBody  = 64 << 10  // job body

	// ReplyCore is the rpl of every token core mints for its own calls.
	ReplyCore = "_INBOX_gateway."
	// replyBFF starts the rpl of a grant: _INBOX_bff.<app>.
	replyBFF = "_INBOX_bff."
)

// Kind is the knd claim: how the message travels.
type Kind string

const (
	KindReq Kind = "req" // request/reply on inference.v1.>
	KindJob Kind = "job" // JetStream on jobs.v1.>
)

// Via is the via claim: who asked for the call (spec §17.2).
type Via string

const (
	ViaCore  Via = "core"  // core called
	ViaGrant Via = "grant" // core approved a BFF-direct call
)

// Verification errors, one per shared reason code (Reason). They are fixed
// values: none holds the token, a claim or a path.
var (
	ErrSize         = errors.New("reqtoken: token or body too large")
	ErrMalformed    = errors.New("reqtoken: malformed")
	ErrSignature    = errors.New("reqtoken: bad signature, header or unknown key")
	ErrClaims       = errors.New("reqtoken: wrong issuer, audience, lifetime or claims")
	ErrExpired      = errors.New("reqtoken: expired")
	ErrBinding      = errors.New("reqtoken: not bound to this subject, kind or body")
	ErrReplySubject = errors.New("reqtoken: reply subject outside the token's reply prefix")
)

// Mint errors.
var (
	ErrMintClaims   = errors.New("reqtoken: refused to mint: missing or invalid claims")
	ErrMintTTL      = errors.New("reqtoken: refused to mint: lifetime under a second or over the cap for its kind")
	ErrMintHospital = errors.New("reqtoken: refused to mint: a hospital id is required outside jobs.v1.global")
	errNoKey        = errors.New("reqtoken: signer has no key")
	errKey          = errors.New("reqtoken: cannot load the request-token signing key (want a PEM PKCS#8 Ed25519 private key)")
)

// Reason maps a verification error to the reason code shared with the
// Python verifier (spec §7.3, plan Task 8): "ok" for nil.
func Reason(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrSize):
		return "size"
	case errors.Is(err, ErrSignature):
		return "signature"
	case errors.Is(err, ErrClaims):
		return "claims"
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrBinding):
		return "binding"
	case errors.Is(err, ErrReplySubject):
		return "reply_subject"
	default:
		return "malformed"
	}
}

// Claims are a request token's claims. On Mint, Jti, BodyHash, KeyID,
// IssuedAt and ExpiresAt are set by the signer; anything passed in them is
// ignored.
type Claims struct {
	Aud         string   // aud: the service
	Sub         string   // sub: the user id, or SvcCore
	Hid         string   // hid: "" only on jobs.v1.global.*
	Roles       []string // roles at mint time
	Rid         string   // rid: the request id
	Jti         string   // jti: 128 random bits, base64url
	Kind        Kind     // knd
	Subject     string   // nsub: the exact subject
	BodyHash    string   // bh: base64url(sha256(body))
	PatientIDs  []string // pids, omitted when empty
	ReplyPrefix string   // rpl: required on req, absent on job
	Via         Via      // via

	KeyID               string // the header kid
	IssuedAt, ExpiresAt time.Time
}

// Expect is what the verifier knows about the message: its own service,
// the subject it arrived on and how it arrived.
type Expect struct {
	Aud, Subject string
	Kind         Kind
}

// Signer mints request tokens. Now defaults to time.Now.
type Signer struct {
	Key   ed25519.PrivateKey
	KeyID string
	Now   func() time.Time

	rand io.Reader // tests only (the fixed vectors); nil means crypto/rand
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

type payload struct {
	Iss   string   `json:"iss"`
	Aud   string   `json:"aud"`
	Sub   string   `json:"sub"`
	Hid   string   `json:"hid"`
	Roles []string `json:"roles"`
	Rid   string   `json:"rid"`
	Jti   string   `json:"jti"`
	Iat   int64    `json:"iat"`
	Exp   int64    `json:"exp"`
	Knd   string   `json:"knd"`
	Nsub  string   `json:"nsub"`
	Bh    string   `json:"bh"`
	Pids  []string `json:"pids,omitempty"`
	Rpl   string   `json:"rpl,omitempty"`
	Via   string   `json:"via"`
}

var (
	headerKeys = map[string]bool{"alg": true, "typ": true, "kid": true}
	// payloadKeys: true for a required claim, false for an optional one.
	payloadKeys = map[string]bool{"iss": true, "aud": true, "sub": true, "hid": true, "roles": true, "rid": true,
		"jti": true, "iat": true, "exp": true, "knd": true, "nsub": true, "bh": true, "via": true,
		"pids": false, "rpl": false}
)

// b64 is unpadded base64url, strict: non-canonical trailing bits are refused.
var b64 = base64.RawURLEncoding.Strict()

// KeyID is the key id rule shared with token.LoadIssuer: the first 16 hex
// characters of sha256(public key).
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// LoadSigner reads the request-token signing key (REQUEST_KEY_PATH): the
// same PEM PKCS#8 Ed25519 format and key id rule as token.LoadIssuer.
func LoadSigner(path string) (Signer, error) {
	iss, err := token.LoadIssuer(path)
	if err != nil {
		return Signer{}, errKey
	}
	return Signer{Key: iss.Key, KeyID: iss.KeyID}, nil
}

// PublicKeys returns the verification key set for this signer.
func (s Signer) PublicKeys() map[string]ed25519.PublicKey {
	if len(s.Key) != ed25519.PrivateKeySize {
		return nil
	}
	return map[string]ed25519.PublicKey{s.KeyID: s.Key.Public().(ed25519.PublicKey)}
}

func (s Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func maxTTL(k Kind) (time.Duration, bool) {
	switch k {
	case KindReq:
		return MaxReqTTL, true
	case KindJob:
		return MaxJobTTL, true
	}
	return 0, false
}

func maxBody(k Kind) (int, bool) {
	switch k {
	case KindReq:
		return MaxReqBody, true
	case KindJob:
		return MaxJobBody, true
	}
	return 0, false
}

// checkSizes is check 1 (spec §7.3), the one size check: Mint runs it on
// what it made and VerifyAt before anything else.
func checkSizes(tok string, body []byte, k Kind) error {
	limit, ok := maxBody(k)
	if !ok {
		return ErrBinding
	}
	if len(tok) > MaxTokenLen || len(body) > limit {
		return ErrSize
	}
	return nil
}

func bodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return b64.EncodeToString(sum[:])
}

// Mint signs c for body, valid for ttl (rounded down to whole seconds, at
// least 1 s, at most the cap for c.Kind). It returns the token and the
// claims as signed.
func (s Signer) Mint(c Claims, body []byte, ttl time.Duration) (string, Claims, error) {
	if len(s.Key) != ed25519.PrivateKeySize || s.KeyID == "" {
		return "", Claims{}, errNoKey
	}
	if err := validateMint(c); err != nil {
		return "", Claims{}, err
	}
	limit, _ := maxTTL(c.Kind) // validateMint checked the kind
	secs := ttl / time.Second
	if ttl > limit || secs < 1 {
		return "", Claims{}, ErrMintTTL
	}
	var nonce [16]byte
	r := s.rand
	if r == nil {
		r = rand.Reader
	}
	if _, err := io.ReadFull(r, nonce[:]); err != nil {
		return "", Claims{}, errors.New("reqtoken: no randomness")
	}
	iat := time.Unix(s.now().Unix(), 0).UTC()
	exp := iat.Add(secs * time.Second)
	roles := c.Roles
	if roles == nil {
		roles = []string{}
	}
	out := c
	out.Roles, out.Jti, out.BodyHash, out.KeyID = roles, b64.EncodeToString(nonce[:]), bodyHash(body), s.KeyID
	out.IssuedAt, out.ExpiresAt = iat, exp
	var pids []string
	if len(c.PatientIDs) > 0 {
		pids = c.PatientIDs
	}
	h, err := json.Marshal(header{Alg: "EdDSA", Typ: Type, Kid: s.KeyID})
	if err != nil {
		return "", Claims{}, err
	}
	p, err := json.Marshal(payload{Iss: Issuer, Aud: c.Aud, Sub: c.Sub, Hid: c.Hid, Roles: roles, Rid: c.Rid,
		Jti: out.Jti, Iat: iat.Unix(), Exp: exp.Unix(), Knd: string(c.Kind), Nsub: c.Subject, Bh: out.BodyHash,
		Pids: pids, Rpl: c.ReplyPrefix, Via: string(c.Via)})
	if err != nil {
		return "", Claims{}, err
	}
	in := b64.EncodeToString(h) + "." + b64.EncodeToString(p)
	tok := in + "." + b64.EncodeToString(ed25519.Sign(s.Key, []byte(in)))
	if err := checkSizes(tok, body, c.Kind); err != nil {
		return "", Claims{}, err
	}
	return tok, out, nil
}

// validateMint refuses claims core must never sign: core fails closed on
// its own mistakes before a worker has to.
func validateMint(c Claims) error {
	if c.Aud == "" || c.Sub == "" || c.Rid == "" || c.Subject == "" {
		return ErrMintClaims
	}
	if _, ok := maxTTL(c.Kind); !ok || !isName(c.Aud) {
		return ErrMintClaims
	}
	global := strings.HasPrefix(c.Subject, "jobs.v1.global.")
	if c.Hid == "" && !global {
		return ErrMintHospital // F10, spec §7.2
	}
	if c.Hid != "" && !isUUID(c.Hid) {
		return ErrMintClaims
	}
	if c.Sub == SvcCore && len(c.Roles) > 0 {
		return ErrMintClaims // spec §7.2: roles are empty for svc:core
	}
	seg := strings.Split(c.Subject, ".")
	switch c.Kind {
	case KindReq:
		// inference.v1.<service>.<op>, and the service is the audience.
		if len(seg) != 4 || seg[0] != "inference" || seg[1] != "v1" || seg[2] != c.Aud || !isName(seg[3]) {
			return ErrMintClaims
		}
		switch c.Via {
		case ViaCore:
			if c.ReplyPrefix != ReplyCore {
				return ErrMintClaims
			}
		case ViaGrant:
			if !isBFFPrefix(c.ReplyPrefix) {
				return ErrMintClaims
			}
		default:
			return ErrMintClaims
		}
	case KindJob:
		// jobs.v1.<hid|global>.<type>; only core publishes jobs, and a job
		// has no reply.
		if len(seg) != 4 || seg[0] != "jobs" || seg[1] != "v1" || !isName(seg[3]) || c.Via != ViaCore || c.ReplyPrefix != "" {
			return ErrMintClaims
		}
		if !global && seg[2] != c.Hid {
			return ErrMintClaims
		}
	}
	return nil
}

// VerifyAt runs checks 1-4 of spec §7.3 on tok for a message with body that
// arrived as want says, at the time now, and returns the claims. Check 5
// (the reply subject) is CheckReply.
func VerifyAt(tok string, keys map[string]ed25519.PublicKey, want Expect, body []byte, now time.Time) (Claims, error) {
	// 1. Sizes.
	if err := checkSizes(tok, body, want.Kind); err != nil {
		return Claims{}, err
	}
	// 2. Shape and signature.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}
	hraw, err1 := b64.DecodeString(parts[0])
	praw, err2 := b64.DecodeString(parts[1])
	sig, err3 := b64.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Claims{}, ErrMalformed
	}
	var h header
	if present, err := strictObject(hraw, headerKeys, &h); err != nil || len(present) != len(headerKeys) {
		return Claims{}, ErrMalformed
	}
	if h.Alg != "EdDSA" || h.Typ != Type {
		return Claims{}, ErrSignature
	}
	key, ok := keys[h.Kid]
	if !ok || len(key) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return Claims{}, ErrSignature
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrSignature
	}
	var p payload
	present, err := strictObject(praw, payloadKeys, &p)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	// 3. Claims.
	for k, required := range payloadKeys {
		if required && !present[k] {
			return Claims{}, ErrClaims
		}
	}
	knd, via := Kind(p.Knd), Via(p.Via)
	limit, ok := maxTTL(knd)
	if !ok || p.Iss != Issuer || p.Aud == "" || p.Aud != want.Aud || p.Sub == "" || p.Rid == "" || p.Jti == "" ||
		p.Nsub == "" || p.Bh == "" {
		return Claims{}, ErrClaims
	}
	switch {
	case via != ViaCore && via != ViaGrant,
		knd == KindReq && p.Rpl == "",
		knd == KindJob && (present["rpl"] || via != ViaCore),
		p.Hid == "" && !strings.HasPrefix(p.Nsub, "jobs.v1.global."),
		p.Exp <= p.Iat,
		p.Exp-p.Iat > int64(limit/time.Second),
		p.Iat > now.Unix()+int64(Skew/time.Second):
		return Claims{}, ErrClaims
	}
	if now.Unix() >= p.Exp {
		return Claims{}, ErrExpired
	}
	// 4. Binding.
	if p.Nsub != want.Subject || knd != want.Kind {
		return Claims{}, ErrBinding
	}
	seg := strings.Split(p.Nsub, ".")
	switch knd {
	case KindReq:
		if len(seg) != 4 || seg[0] != "inference" || seg[1] != "v1" {
			return Claims{}, ErrBinding
		}
	case KindJob:
		if len(seg) != 4 || seg[0] != "jobs" || seg[1] != "v1" || (seg[2] != "global" && seg[2] != p.Hid) {
			return Claims{}, ErrBinding
		}
	}
	if subtle.ConstantTimeCompare([]byte(p.Bh), []byte(bodyHash(body))) != 1 {
		return Claims{}, ErrBinding
	}
	return Claims{Aud: p.Aud, Sub: p.Sub, Hid: p.Hid, Roles: p.Roles, Rid: p.Rid, Jti: p.Jti, Kind: knd,
		Subject: p.Nsub, BodyHash: p.Bh, PatientIDs: p.Pids, ReplyPrefix: p.Rpl, Via: via, KeyID: h.Kid,
		IssuedAt: time.Unix(p.Iat, 0).UTC(), ExpiresAt: time.Unix(p.Exp, 0).UTC()}, nil
}

// CheckReply is check 5 of spec §7.3 for request/reply. granted is false
// for a token found in the message itself, which only core may put there
// (via core, rpl _INBOX_gateway.); true for the token core returned for a
// grant (via grant, rpl _INBOX_bff.<app>.). The reply subject must then
// start with the token's rpl. A failure means: refuse, and send no reply.
func CheckReply(c Claims, reply string, granted bool) error {
	if c.Kind != KindReq {
		return ErrReplySubject
	}
	if granted {
		if c.Via != ViaGrant || !isBFFPrefix(c.ReplyPrefix) {
			return ErrReplySubject
		}
	} else if c.Via != ViaCore || c.ReplyPrefix != ReplyCore {
		return ErrReplySubject
	}
	if len(reply) <= len(c.ReplyPrefix) || !strings.HasPrefix(reply, c.ReplyPrefix) {
		return ErrReplySubject
	}
	return nil
}

// strictObject decodes raw, a single JSON object, into v. It refuses
// invalid UTF-8, a key not in allowed (matched exactly: encoding/json alone
// folds case; nil allows any key), a repeated key, a null value and
// trailing data, and returns the keys present.
func strictObject(raw []byte, allowed map[string]bool, v any) (map[string]bool, error) {
	errStrict := errors.New("not strict JSON")
	if !utf8.Valid(raw) {
		return nil, errStrict
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errStrict
	}
	present := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, errStrict
		}
		k, ok := t.(string)
		_, known := allowed[k]
		if !ok || present[k] || (allowed != nil && !known) {
			return nil, errStrict
		}
		present[k] = true
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil || string(val) == "null" {
			return nil, errStrict // encoding/json would read null as "" or nil
		}
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, errStrict
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errStrict
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, errStrict
	}
	return present, nil
}

// isName is a subject token: lower-case [a-z0-9_]+ (spec §5.1).
func isName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// isUUID is the canonical lower-case uuid form used in subjects.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return false
			}
		}
	}
	return true
}

// isBFFPrefix is _INBOX_bff.<app>. with <app> a subject name.
func isBFFPrefix(p string) bool {
	if !strings.HasPrefix(p, replyBFF) || !strings.HasSuffix(p, ".") {
		return false
	}
	return isName(strings.TrimSuffix(strings.TrimPrefix(p, replyBFF), "."))
}
