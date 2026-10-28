package reqtoken

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb"
)

var update = flag.Bool("update", false, "rewrite the shared request-token vectors in testdata/request-tokens")

// vectorDir is the repository's testdata/request-tokens, which the Python
// verifier's test_token_vectors.py also reads.
var vectorDir = filepath.Join("..", "..", "..", "..", "testdata", "request-tokens")

// testOnlyVectorSeed is a TEST-ONLY Ed25519 seed. It signs the shared
// vectors and nothing else: no key file is ever written from it, so core
// serve and the workers can never load it. Its public half is inside each
// vector file.
var testOnlyVectorSeed = sha256.Sum256([]byte("mhai test-only request-token vector seed; never a real key"))

// The fixed clock: every vector is minted at vectorMint.
var vectorMint = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// vector is one shared test case. Its verdict covers checks 1-5 of spec
// §7.3 (sizes, shape and signature, claims, binding, reply subject), the
// checks the token alone decides. Valid vectors also carry the matching
// request_id and schema headers, and a body that names the token's
// hospital, so checks 6-8 pass on them too.
type vector struct {
	Name      string            `json:"name"`
	Note      string            `json:"note"`
	Token     string            `json:"token"`
	Keys      map[string]string `json:"keys"`       // kid -> base64url public key
	Body      string            `json:"body"`       // standard base64 of the body bytes
	Subject   string            `json:"subject"`    // the subject the message arrived on
	Reply     string            `json:"reply"`      // the reply subject ("" for jobs)
	RequestID string            `json:"request_id"` // the Mhai-Request-Id header
	Schema    string            `json:"schema"`     // the Mhai-Schema header
	Knd       string            `json:"knd"`        // how it arrived: req (core subject) or job (JetStream)
	Granted   bool              `json:"granted"`    // true: the token core returned for a grant; false: found in the message
	Service   string            `json:"service"`    // the verifying service (aud)
	Now       int64             `json:"now"`        // unix seconds
	Verdict   string            `json:"verdict"`    // ok, or the shared reason code
}

func vectorSigner(name string) Signer {
	priv := ed25519.NewKeyFromSeed(testOnlyVectorSeed[:])
	jti := sha256.Sum256([]byte("jti:" + name))
	return Signer{Key: priv, KeyID: KeyID(priv.Public().(ed25519.PublicKey)),
		Now: func() time.Time { return vectorMint }, rand: bytes.NewReader(jti[:16])}
}

func pb(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// buildVectors makes every vector from the fixed key and clock. Same
// inputs, same bytes: ed25519 signatures are deterministic and the jti
// comes from the vector's name.
func buildVectors(t *testing.T) []vector {
	t.Helper()
	const (
		pingSchema = "mhai.intelligence.v1.PingRequest"
		jobSchema  = "mhai.intelligence.v1.RunQueryJob"
		coreReply  = "_INBOX_gateway.vectors.1"
		bffReply   = "_INBOX_bff.clinical.vectors.1"
		queryID    = "66666666-6666-4666-8666-666666666666"
		bffRid     = "77777777-7777-4777-8777-777777777777"
	)
	priv := ed25519.NewKeyFromSeed(testOnlyVectorSeed[:])
	pub := priv.Public().(ed25519.PublicKey)
	kid := KeyID(pub)
	keySet := map[string]string{kid: b64.EncodeToString(pub)}
	ping := pb(t, &intelpb.PingRequest{Text: "synthetic ping", HospitalId: testHospital})
	jobBody := pb(t, &intelpb.RunQueryJob{QueryId: queryID})
	jobSubject := "jobs.v1." + testHospital + ".run_query"
	after := vectorMint.Add(time.Second).Unix()

	reqC := Claims{Aud: "echo", Sub: testUser, Hid: testHospital, Roles: []string{"clinician"}, Rid: testRid,
		Kind: KindReq, Subject: "inference.v1.echo.ping", ReplyPrefix: ReplyCore, Via: ViaCore}
	jobC := Claims{Aud: "pipeline", Sub: testUser, Hid: testHospital, Roles: []string{"clinician"}, Rid: testRid,
		Kind: KindJob, Subject: jobSubject, Via: ViaCore}
	globalC := Claims{Aud: "pipeline", Sub: SvcCore, Rid: testRid, Kind: KindJob,
		Subject: "jobs.v1.global.retention_purge", Via: ViaCore}
	grantC := reqC
	grantC.Rid, grantC.Via, grantC.ReplyPrefix = bffRid, ViaGrant, "_INBOX_bff.clinical."

	mintV := func(name string, c Claims, body []byte, ttl time.Duration) string {
		tok, _, err := vectorSigner(name).Mint(c, body, ttl)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return tok
	}
	resignV := func(name string, c Claims, body []byte, ttl time.Duration, edit func(h, p map[string]any)) string {
		return resign(t, vectorSigner(name), mintV(name, c, body, ttl), edit)
	}
	req := func(name, note, tok, verdict string) vector {
		return vector{Name: name, Note: note, Token: tok, Keys: keySet, Body: base64.StdEncoding.EncodeToString(ping),
			Subject: reqC.Subject, Reply: coreReply, RequestID: testRid, Schema: pingSchema, Knd: "req",
			Service: "echo", Now: after, Verdict: verdict}
	}
	job := func(name, note, tok, verdict string) vector {
		return vector{Name: name, Note: note, Token: tok, Keys: keySet, Body: base64.StdEncoding.EncodeToString(jobBody),
			Subject: jobSubject, RequestID: testRid, Schema: jobSchema, Knd: "job",
			Service: "pipeline", Now: after, Verdict: verdict}
	}
	reqTok := func(name string) string { return mintV(name, reqC, ping, 5*time.Second) }
	// rawV re-signs tok with its payload JSON edited as text (for JSON that
	// a map can't express: repeats, case, null, floats).
	rawV := func(tok string, edit func(p string) string) string {
		parts := strings.Split(tok, ".")
		h, _ := b64.DecodeString(parts[0])
		pl, _ := b64.DecodeString(parts[1])
		return signRawBytes(priv, string(h), edit(string(pl)))
	}
	iatJSON := fmt.Sprintf(`"iat":%d`, vectorMint.Unix())
	edited := func(name, note, old, repl string) vector {
		return req(name, note, rawV(reqTok(name), func(p string) string { return strings.Replace(p, old, repl, 1) }), "malformed")
	}
	split := func(name string) []string { return strings.Split(reqTok(name), ".") }

	var vs []vector
	vs = append(vs,
		req("valid_req", "core's own request/reply call", reqTok("valid_req"), "ok"),
		job("valid_job", "a hospital job on JetStream", mintV("valid_job", jobC, jobBody, 10*time.Minute), "ok"),
		func() vector {
			v := job("valid_global_job", "a job with no hospital; schema and body empty (no such operation yet)",
				mintV("valid_global_job", globalC, nil, time.Hour), "ok")
			v.Subject, v.Body, v.Schema = globalC.Subject, "", ""
			return v
		}(),
		func() vector {
			v := req("valid_grant", "the token core returned for a BFF-direct call", mintV("valid_grant", grantC, ping, 2*time.Second), "ok")
			v.Reply, v.RequestID, v.Granted = bffReply, bffRid, true
			return v
		}(),
		func() vector {
			tok := reqTok("tampered_payload")
			forged := resign(t, vectorSigner("tampered_payload"), tok, func(_, p map[string]any) { p["roles"] = []string{"platform_admin"} })
			parts := strings.Split(tok, ".")
			return req("tampered_payload", "payload changed after signing", parts[0]+"."+strings.Split(forged, ".")[1]+"."+parts[2], "signature")
		}(),
		req("wrong_typ", "typ JWT (an access token's), validly signed",
			resignV("wrong_typ", reqC, ping, 5*time.Second, func(h, _ map[string]any) { h["typ"] = "JWT" }), "signature"),
		func() vector {
			s := vectorSigner("unknown_kid")
			s.KeyID = "0000000000000000"
			tok, _, err := s.Mint(reqC, ping, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			return req("unknown_kid", "kid not in the trusted set", tok, "signature")
		}(),
		func() vector {
			v := req("wrong_aud", "a token for echo verified by pipeline", reqTok("wrong_aud"), "claims")
			v.Service = "pipeline"
			return v
		}(),
		func() vector {
			v := req("expired", "now equals exp", reqTok("expired"), "expired")
			v.Now = vectorMint.Add(5 * time.Second).Unix()
			return v
		}(),
		req("exp_too_far_req", "exp 61 s after iat on a req token",
			resignV("exp_too_far_req", reqC, ping, 5*time.Second, func(_, p map[string]any) { p["exp"] = vectorMint.Add(61 * time.Second).Unix() }), "claims"),
		job("exp_too_far_job", "exp 25 h after iat on a job token",
			resignV("exp_too_far_job", jobC, jobBody, time.Minute, func(_, p map[string]any) { p["exp"] = vectorMint.Add(25 * time.Hour).Unix() }), "claims"),
		func() vector {
			v := req("iat_in_future", "iat 6 s after now (the skew is 5 s)",
				resignV("iat_in_future", reqC, ping, 5*time.Second, func(_, p map[string]any) {
					p["iat"] = vectorMint.Add(6 * time.Second).Unix()
					p["exp"] = vectorMint.Add(11 * time.Second).Unix()
				}), "claims")
			v.Now = vectorMint.Unix()
			return v
		}(),
		func() vector {
			v := req("wrong_nsub", "the token is for echo.ping, the message came on echo.other", reqTok("wrong_nsub"), "binding")
			v.Subject = "inference.v1.echo.other"
			return v
		}(),
		func() vector {
			v := req("wrong_bh", "the body differs from the one signed", reqTok("wrong_bh"), "binding")
			v.Body = base64.StdEncoding.EncodeToString(pb(t, &intelpb.PingRequest{Text: "synthetic pinG", HospitalId: testHospital}))
			return v
		}(),
		func() vector {
			v := job("job_hospital_mismatch", "nsub names another hospital than hid",
				resignV("job_hospital_mismatch", jobC, jobBody, time.Minute, func(_, p map[string]any) {
					p["nsub"] = "jobs.v1." + otherHosp + ".run_query"
				}), "binding")
			v.Subject = "jobs.v1." + otherHosp + ".run_query"
			return v
		}(),
		func() vector {
			v := req("wrong_knd", "a req token arriving as a job", reqTok("wrong_knd"), "binding")
			v.Knd, v.Reply = "job", ""
			return v
		}(),
		func() vector {
			v := req("reply_outside_rpl", "the reply goes to a worker's inbox, not _INBOX_gateway.", reqTok("reply_outside_rpl"), "reply_subject")
			v.Reply = "_INBOX_worker.echo.vectors.1"
			return v
		}(),
		func() vector {
			v := req("grant_in_message", "a via=grant token presented in Mhai-Request-Token: a BFF can't bring its own",
				mintV("grant_in_message", grantC, ping, 2*time.Second), "reply_subject")
			v.Reply, v.RequestID = bffReply, bffRid
			return v
		}(),
		func() vector {
			v := req("grant_other_inbox", "a granted token whose reply goes to another app's inbox",
				mintV("grant_other_inbox", grantC, ping, 2*time.Second), "reply_subject")
			v.Reply, v.RequestID, v.Granted = "_INBOX_bff.platform.vectors.1", bffRid, true
			return v
		}(),
		func() vector {
			parts := strings.Split(reqTok("alg_none"), ".")
			h, _ := json.Marshal(map[string]string{"alg": "none", "typ": Type, "kid": kid})
			return req("alg_none", "alg none, empty signature", b64.EncodeToString(h)+"."+parts[1]+".", "signature")
		}(),
		func() vector {
			parts := strings.Split(reqTok("alg_hs256"), ".")
			h, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": Type, "kid": kid})
			in := b64.EncodeToString(h) + "." + parts[1]
			mac := hmac.New(sha256.New, pub)
			mac.Write([]byte(in))
			return req("alg_hs256", "alg HS256, HMAC keyed with the public key", in+"."+b64.EncodeToString(mac.Sum(nil)), "signature")
		}(),
		req("wrong_iss", "iss mhai-core",
			resignV("wrong_iss", reqC, ping, 5*time.Second, func(_, p map[string]any) { p["iss"] = "mhai-core" }), "claims"),
		req("missing_rid", "no rid claim",
			resignV("missing_rid", reqC, ping, 5*time.Second, func(_, p map[string]any) { delete(p, "rid") }), "claims"),
		req("unknown_claim", "an extra claim adm",
			resignV("unknown_claim", reqC, ping, 5*time.Second, func(_, p map[string]any) { p["adm"] = true }), "malformed"),
		func() vector {
			pids := make([]string, 120)
			for i := range pids {
				pids[i] = testPatient
			}
			return req("oversize", "a validly signed token over 4096 bytes",
				resignV("oversize", reqC, ping, 5*time.Second, func(_, p map[string]any) { p["pids"] = pids }), "size")
		}(),
		// Strict parsing: what encoding/json alone would let through.
		req("dup_payload_key", "hid repeated with another hospital's id",
			rawV(reqTok("dup_payload_key"), func(p string) string {
				return strings.TrimSuffix(p, "}") + `,"hid":"` + otherHosp + `"}`
			}), "malformed"),
		edited("case_folded_claim", "HID instead of hid", `"hid"`, `"HID"`),
		edited("null_claim", "rid is null", `"rid":"`+testRid+`"`, `"rid":null`),
		edited("float_iat", "iat written as a float", iatJSON, iatJSON+".0"),
		edited("bool_iat", "iat is true", iatJSON, `"iat":true`),
		edited("null_in_roles", "roles holds a null", `"roles":["clinician"]`, `"roles":[null]`),
		func() vector {
			p := split("padded_segment")
			return req("padded_segment", "the header segment carries base64 padding", p[0]+"=."+p[1]+"."+p[2], "malformed")
		}(),
		func() vector {
			p := split("four_parts")
			return req("four_parts", "a fourth segment", strings.Join(p, ".")+"."+p[2], "malformed")
		}(),
		func() vector {
			p := split("newline_in_segment")
			return req("newline_in_segment", "LF inside the signature segment (base64 decoders skip it)",
				p[0]+"."+p[1]+"."+p[2][:20]+"\n"+p[2][20:], "malformed")
		}(),
		req("iat_overflow", "iat -9e18 and exp 9e18: exp - iat overflows int64",
			resignV("iat_overflow", reqC, ping, 5*time.Second, func(_, p map[string]any) {
				p["iat"], p["exp"] = int64(-9e18), int64(9e18)
			}), "claims"),
		// Boundaries.
		func() vector {
			v := req("iat_at_skew", "iat exactly 5 s after now: within the skew",
				resignV("iat_at_skew", reqC, ping, 5*time.Second, func(_, p map[string]any) {
					p["iat"] = vectorMint.Add(Skew).Unix()
					p["exp"] = vectorMint.Add(Skew + 5*time.Second).Unix()
				}), "ok")
			v.Now = vectorMint.Unix()
			return v
		}(),
		req("exp_iat_60s", "exp - iat exactly 60 s, the req cap", mintV("exp_iat_60s", reqC, ping, MaxReqTTL), "ok"),
		req("empty_hid_hospital_subject", "hid empty on an inference subject",
			resignV("empty_hid_hospital_subject", reqC, ping, 5*time.Second, func(_, p map[string]any) { p["hid"] = "" }), "claims"),
		job("rpl_on_job", "a job token carrying rpl",
			resignV("rpl_on_job", jobC, jobBody, time.Minute, func(_, p map[string]any) { p["rpl"] = ReplyCore }), "claims"),
		func() vector {
			v := job("oversize_body", "a job body one byte over 64 KB", mintV("oversize_body", jobC, jobBody, time.Minute), "size")
			v.Body = base64.StdEncoding.EncodeToString(make([]byte, MaxJobBody+1))
			return v
		}(),
	)
	sort.Slice(vs, func(i, j int) bool { return vs[i].Name < vs[j].Name })
	return vs
}

// evaluate runs Go's verifier (checks 1-5) on v and returns the reason.
func evaluate(t *testing.T, v vector) string {
	t.Helper()
	keys := map[string]ed25519.PublicKey{}
	for kid, k := range v.Keys {
		raw, err := b64.DecodeString(k)
		if err != nil {
			t.Fatal(err)
		}
		keys[kid] = raw
	}
	body, err := base64.StdEncoding.DecodeString(v.Body)
	if err != nil {
		t.Fatal(err)
	}
	c, err := VerifyAt(v.Token, keys, Expect{Aud: v.Service, Subject: v.Subject, Kind: Kind(v.Knd)}, body, time.Unix(v.Now, 0))
	if err == nil && Kind(v.Knd) == KindReq {
		err = CheckReply(c, v.Reply, v.Granted)
	}
	if err == nil && (c.Rid != v.RequestID) {
		t.Fatalf("%s: a valid vector's request_id must equal rid", v.Name)
	}
	return Reason(err)
}

// A25: the vectors on disk are exactly what the fixed key and clock make,
// and Go's verifier gives each its stated verdict.
func TestRequestTokenVectorsUpToDate(t *testing.T) {
	vs := buildVectors(t)
	want := map[string][]byte{}
	for _, v := range vs {
		if got := evaluate(t, v); got != v.Verdict {
			t.Errorf("%s: Go says %q, vector says %q", v.Name, got, v.Verdict)
		}
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		want[v.Name+".json"] = append(raw, '\n')
	}
	if *update {
		if err := os.MkdirAll(vectorDir, 0o755); err != nil {
			t.Fatal(err)
		}
		old, _ := filepath.Glob(filepath.Join(vectorDir, "*.json"))
		for _, f := range old {
			if _, keep := want[filepath.Base(f)]; !keep {
				_ = os.Remove(f)
			}
		}
		for name, raw := range want {
			if err := os.WriteFile(filepath.Join(vectorDir, name), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	onDisk, _ := filepath.Glob(filepath.Join(vectorDir, "*.json"))
	if len(onDisk) != len(want) {
		t.Errorf("%d vector files on disk, want %d (run with -update)", len(onDisk), len(want))
	}
	for name, raw := range want {
		got, err := os.ReadFile(filepath.Join(vectorDir, name))
		if err != nil || !bytes.Equal(got, raw) {
			t.Errorf("%s is stale or missing (run go test ./internal/reqtoken -run TestRequestTokenVectorsUpToDate -update)", name)
		}
	}
	// Every required kind of case is covered.
	for _, name := range []string{"valid_req", "valid_job", "valid_global_job", "tampered_payload", "wrong_typ",
		"unknown_kid", "wrong_aud", "expired", "exp_too_far_req", "exp_too_far_job", "wrong_nsub", "wrong_bh",
		"reply_outside_rpl", "grant_in_message", "alg_none", "alg_hs256", "wrong_iss", "missing_rid",
		"unknown_claim", "oversize", "dup_payload_key", "case_folded_claim", "null_claim", "float_iat", "bool_iat",
		"null_in_roles", "padded_segment", "four_parts", "newline_in_segment", "iat_overflow", "iat_at_skew",
		"exp_iat_60s", "empty_hid_hospital_subject", "rpl_on_job", "oversize_body"} {
		if _, ok := want[name+".json"]; !ok {
			t.Errorf("no %s vector", name)
		}
	}
}
