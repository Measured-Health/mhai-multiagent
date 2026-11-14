package synth

// The synthetic FHIR R4 server: POST <token> checks a SMART Backend
// Services client assertion (RS384 or ES384, against the one registered
// public key; iss and sub the client id; aud the token address; a 5-minute
// exp; a jti used once) and issues a bearer token; GET <base>/metadata is
// an R4 capability statement; GET <base>/<Type> searches by _lastUpdated
// (sorted, paged with absolute next links) or reads by _id.

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// ClientID is the synthetic client's id.
const ClientID = "synthetic-client"

// The synthetic connection and hospital (the connection is the one
// maps/testdata/synthetic/fhir.yaml maps).
var (
	Connection = uuid.MustParse("00000000-0000-4000-8000-00000000f41a")
	Hospital   = uuid.MustParse("00000000-0000-4000-8000-0000000051a0")
)

// Options configure the handler.
type Options struct {
	// BaseURL and TokenURL are the server's public addresses.
	BaseURL, TokenURL string
	// Pub is the registered client key.
	Pub crypto.PublicKey
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// Late makes Observations, last updated at the time given, that are
	// served only from LateAfter after the first Observation search (a
	// connection's first sync, whose backfill is historical by design): a
	// fresh result arriving later (synth-fhir --late-critical). Nil: none.
	Late      func(at time.Time) []map[string]any
	LateAfter time.Duration
}

type store struct {
	opts   Options
	byType map[string][]map[string]any
	mu     sync.Mutex
	tokens map[string]time.Time
	jtis   map[string]time.Time
	// firstObs is the first Observation search (Options.Late).
	firstObs time.Time
}

// Handler serves ps (and the synthetic wards).
func Handler(ps []Patient, o Options) http.Handler {
	s := &store{opts: o, byType: map[string][]map[string]any{}, tokens: map[string]time.Time{}, jtis: map[string]time.Time{}}
	anchor := time.Now().UTC()
	s.byType["Location"] = Locations(anchor)
	for _, p := range ps {
		s.byType["Patient"] = append(s.byType["Patient"], p.Resource)
		s.byType["Encounter"] = append(s.byType["Encounter"], p.Encounter)
		s.byType["Observation"] = append(s.byType["Observation"], p.Observations...)
		for _, m := range p.Medications {
			t, _ := m["resourceType"].(string)
			s.byType[t] = append(s.byType[t], m)
		}
	}
	for t := range s.byType {
		sort.SliceStable(s.byType[t], func(i, j int) bool { return lastUpdated(s.byType[t][i]) < lastUpdated(s.byType[t][j]) })
	}
	return s
}

// late is Options.Late's Observations once they are due: the first
// Observation search starts the clock (and is served without them).
func (s *store) late() []map[string]any {
	if s.opts.Late == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.firstObs.IsZero() {
		s.firstObs = now
	}
	at := s.firstObs.Add(s.opts.LateAfter).UTC().Truncate(time.Second)
	if now.Before(at) {
		return nil
	}
	return s.opts.Late(at)
}

func (s *store) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

func lastUpdated(r map[string]any) string {
	m, _ := r["meta"].(map[string]any)
	v, _ := m["lastUpdated"].(string)
	return v
}

func (s *store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tu, _ := url.Parse(s.opts.TokenURL)
	bu, _ := url.Parse(s.opts.BaseURL)
	switch {
	case tu != nil && r.URL.Path == tu.Path && r.Method == http.MethodPost:
		s.token(w, r)
		return
	case r.Method != http.MethodGet || bu == nil:
		http.Error(w, "not supported", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorised(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, strings.TrimSuffix(bu.Path, "/")+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rest == "metadata" {
		writeJSON(w, capability())
		return
	}
	if !slices.Contains([]string{"Patient", "Observation", "MedicationRequest", "MedicationStatement", "Encounter", "Location"}, rest) {
		http.NotFound(w, r)
		return
	}
	s.search(w, r, rest)
}

func (s *store) authorised(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[tok]
	return ok && s.now().Before(exp)
}

func (s *store) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "client_credentials" ||
		r.PostForm.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" ||
		!s.assertionOK(r.PostForm.Get("client_assertion")) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = s.now().Add(5 * time.Minute)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": tok, "token_type": "bearer", "expires_in": 300, "scope": r.PostForm.Get("scope")})
}

func (s *store) assertionOK(a string) bool {
	parts := strings.Split(a, ".")
	if len(parts) != 3 {
		return false
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	var h struct{ Alg string }
	var c struct {
		Iss, Sub, Aud, Jti string
		Exp                int64
	}
	if json.Unmarshal(hb, &h) != nil || json.Unmarshal(cb, &c) != nil {
		return false
	}
	now := s.now()
	if c.Iss != ClientID || c.Sub != ClientID || c.Aud != s.opts.TokenURL || c.Jti == "" ||
		c.Exp <= now.Unix() || c.Exp > now.Add(6*time.Minute).Unix() {
		return false
	}
	sum := sha512.Sum384([]byte(parts[0] + "." + parts[1]))
	switch k := s.opts.Pub.(type) {
	case *rsa.PublicKey:
		if h.Alg != "RS384" || rsa.VerifyPKCS1v15(k, crypto.SHA384, sum[:], sig) != nil {
			return false
		}
	case *ecdsa.PublicKey:
		if h.Alg != "ES384" || len(sig) != 96 ||
			!ecdsa.Verify(k, sum[:], new(big.Int).SetBytes(sig[:48]), new(big.Int).SetBytes(sig[48:])) {
			return false
		}
	default:
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, used := s.jtis[c.Jti]; used {
		return false
	}
	s.jtis[c.Jti] = time.Unix(c.Exp, 0)
	return true
}

func (s *store) search(w http.ResponseWriter, r *http.Request, typ string) {
	q := r.URL.Query()
	all := s.byType[typ]
	if typ == "Observation" && q.Get("_summary") == "" {
		all = append(slices.Clone(all), s.late()...)
	}
	if q.Get("_summary") == "count" {
		writeJSON(w, map[string]any{"resourceType": "Bundle", "type": "searchset", "total": len(all)})
		return
	}
	var match []map[string]any
	if ids := q.Get("_id"); ids != "" {
		want := strings.Split(ids, ",")
		for _, res := range all {
			if id, _ := res["id"].(string); slices.Contains(want, id) {
				match = append(match, res)
			}
		}
	} else {
		gt, ok := strings.CutPrefix(q.Get("_lastUpdated"), "gt")
		since, err := time.Parse(time.RFC3339Nano, gt)
		if !ok || err != nil || q.Get("_sort") != "_lastUpdated" {
			http.Error(w, "unsupported search", http.StatusBadRequest)
			return
		}
		cats := strings.Split(q.Get("category"), ",")
		for _, res := range all {
			lu, err := time.Parse(time.RFC3339Nano, lastUpdated(res))
			if err != nil || !lu.After(since) {
				continue
			}
			if typ == "Observation" && q.Get("category") != "" && !slices.Contains(cats, category(res)) {
				continue
			}
			match = append(match, res)
		}
	}
	count := 200
	if n, err := strconv.Atoi(q.Get("_count")); err == nil && n > 0 && n < count {
		count = n
	}
	off, _ := strconv.Atoi(q.Get("_offset"))
	off = max(0, min(off, len(match)))
	end := min(off+count, len(match))
	entries := []map[string]any{}
	for _, res := range match[off:end] {
		entries = append(entries, map[string]any{"resource": res, "search": map[string]any{"mode": "match"}})
	}
	b := map[string]any{"resourceType": "Bundle", "type": "searchset", "entry": entries}
	if end < len(match) && q.Get("_id") == "" {
		nq := r.URL.Query()
		nq.Set("_offset", strconv.Itoa(end))
		b["link"] = []map[string]any{{"relation": "next", "url": strings.TrimSuffix(s.opts.BaseURL, "/") + "/" + typ + "?" + nq.Encode()}}
	}
	writeJSON(w, b)
}

func category(res map[string]any) string {
	cs, _ := res["category"].([]any)
	for _, c := range cs {
		m, _ := c.(map[string]any)
		cd, _ := m["coding"].([]any)
		for _, x := range cd {
			xm, _ := x.(map[string]any)
			if v, _ := xm["code"].(string); v != "" {
				return v
			}
		}
	}
	return ""
}

func capability() map[string]any {
	var res []map[string]any
	for _, t := range []string{"Patient", "Observation", "MedicationRequest", "MedicationStatement", "Encounter", "Location"} {
		res = append(res, map[string]any{"type": t, "interaction": []any{map[string]any{"code": "search-type"}},
			"searchParam": []any{map[string]any{"name": "_lastUpdated", "type": "date"}, map[string]any{"name": "_id", "type": "token"}}})
	}
	return map[string]any{"resourceType": "CapabilityStatement", "status": "active", "kind": "instance", "fhirVersion": "4.0.1",
		"format": []any{"json"}, "rest": []any{map[string]any{"mode": "server", "resource": res,
			"searchParam": []any{map[string]any{"name": "_sort", "type": "special"}}}}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/fhir+json")
	_ = json.NewEncoder(w).Encode(v)
}

type jsonNumber string

func (n jsonNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// ClientKey makes a synthetic client key pair (ECDSA P-384): the PEM
// private key for the connection's credential, and its public key for the
// server.
func ClientKey() ([]byte, crypto.PublicKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), &k.PublicKey, nil
}

// Fake is a synthetic server for tests, with its client key.
type Fake struct {
	*httptest.Server
	ClientKey []byte
}

// Server starts a TLS test server for ps.
func Server(ps []Patient) *Fake {
	key, pub, err := ClientKey()
	if err != nil {
		panic("synth: no key: " + err.Error())
	}
	var h atomic.Value // the handler, once the address is known
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := h.Load().(http.Handler); ok {
			v.ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	srv.StartTLS()
	h.Store(Handler(ps, Options{BaseURL: srv.URL + "/r4", TokenURL: srv.URL + "/token", Pub: pub}))
	return &Fake{Server: srv, ClientKey: key}
}

// Config is the synthetic connection to this server.
func (f *Fake) Config() connector.Config {
	u, _ := url.Parse(f.URL)
	return connector.Config{ConnectionID: Connection, HospitalID: Hospital, Type: "fhir_r4", BaseURL: f.URL + "/r4",
		Settings: map[string]string{"client_id": ClientID, "token_url": f.URL + "/token", "mrn_namespace": MRNSystem},
		Origin:   []emrurl.Origin{emrurl.Origin("https://" + u.Host)}, PollSeconds: 120}
}
