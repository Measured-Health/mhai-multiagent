package fhir

// A test FHIR R4 server: a token endpoint that checks the client
// assertion's signature, a capability statement, searches by
// _lastUpdated with paging, and _id reads. Synthetic data only.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

const clientID = "synthetic-client"

type fakeFHIR struct {
	t   *testing.T
	srv *httptest.Server
	pub crypto.PublicKey

	mu         sync.Mutex
	requests   []string // method path?query, in order
	assertions []string
	tokens     int
	res        map[string][]map[string]any // by resource type
	pageSize   int
	fail       map[string]int    // type -> the page (0-based) answered 500
	status     map[string]int    // type -> an HTTP status for every search
	retryAfter string            // with status 429
	nextLink   map[string]string // type -> the next link to give instead
	extra      int               // entries added beyond the page size
	noSort     bool              // answer in insertion order
	capability map[string]any
}

func newFake(t *testing.T, pub crypto.PublicKey) *fakeFHIR {
	f := &fakeFHIR{t: t, pub: pub, res: map[string][]map[string]any{}, pageSize: 200, fail: map[string]int{},
		status: map[string]int{}, nextLink: map[string]string{}}
	f.capability = capability("4.0.1", nil)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeFHIR) base() string     { return f.srv.URL + "/r4" }
func (f *fakeFHIR) tokenURL() string { return f.srv.URL + "/token" }

func (f *fakeFHIR) origin() []emrurl.Origin {
	u, _ := url.Parse(f.srv.URL)
	return []emrurl.Origin{emrurl.Origin("https://" + u.Host)}
}

func (f *fakeFHIR) cfg() connector.Config {
	return connector.Config{ConnectionID: uuid.MustParse("00000000-0000-4000-8000-00000000f41a"),
		HospitalID: uuid.MustParse("0b1f0000-0000-4000-8000-00000000000a"), Type: ID, BaseURL: f.base(),
		Settings: map[string]string{"client_id": clientID, "token_url": f.tokenURL()}, Origin: f.origin(), PollSeconds: 120}
}

func (f *fakeFHIR) reqs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeFHIR) add(typ string, r map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res[typ] = append(f.res[typ], r)
}

func (f *fakeFHIR) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		f.token(w, r)
		return
	case r.Method != http.MethodGet:
		http.Error(w, "no", http.StatusMethodNotAllowed)
		return
	}
	if !f.authorised(r) {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/r4/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if rest == "metadata" {
		writeJSON(w, f.capability)
		return
	}
	f.search(w, r, rest)
}

func (f *fakeFHIR) authorised(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && strings.HasPrefix(tok, "tok-")
}

func (f *fakeFHIR) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad", http.StatusBadRequest)
		return
	}
	a := r.PostForm.Get("client_assertion")
	f.mu.Lock()
	f.assertions = append(f.assertions, a)
	f.mu.Unlock()
	if r.PostForm.Get("grant_type") != "client_credentials" ||
		r.PostForm.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" ||
		!verifyJWT(a, f.pub) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "invalid_client"})
		return
	}
	f.mu.Lock()
	f.tokens++
	n := f.tokens
	f.mu.Unlock()
	writeJSON(w, map[string]any{"access_token": "tok-" + strconv.Itoa(n), "token_type": "bearer", "expires_in": 300})
}

func (f *fakeFHIR) search(w http.ResponseWriter, r *http.Request, typ string) {
	q := r.URL.Query()
	f.mu.Lock()
	all := slices.Clone(f.res[typ])
	status, fail := f.status[typ], f.fail
	next := f.nextLink[typ]
	ra := f.retryAfter
	size, extra, noSort := f.pageSize, f.extra, f.noSort
	f.mu.Unlock()
	if status != 0 {
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.WriteHeader(status)
		return
	}
	if q.Get("_summary") == "count" {
		writeJSON(w, map[string]any{"resourceType": "Bundle", "type": "searchset", "total": len(all)})
		return
	}
	var match []map[string]any
	if ids := q.Get("_id"); ids != "" {
		want := strings.Split(ids, ",")
		for _, res := range all {
			if slices.Contains(want, res["id"].(string)) {
				match = append(match, res)
			}
		}
	} else {
		gt := strings.TrimPrefix(q.Get("_lastUpdated"), "gt")
		since, err := time.Parse(time.RFC3339Nano, gt)
		if err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		for _, res := range all {
			if lu, _ := time.Parse(time.RFC3339Nano, lastUpdated(res)); lu.After(since) {
				match = append(match, res)
			}
		}
		if !noSort {
			sort.SliceStable(match, func(i, j int) bool { return lastUpdated(match[i]) < lastUpdated(match[j]) })
		}
	}
	off, _ := strconv.Atoi(q.Get("_offset"))
	page := off / max(size, 1)
	if p, ok := fail[typ]; ok && p == page {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	end := min(off+size+extra, len(match))
	if off > len(match) {
		off = len(match)
	}
	entries := []map[string]any{}
	for _, res := range match[off:end] {
		entries = append(entries, map[string]any{"resource": res, "search": map[string]any{"mode": "match"}})
	}
	b := map[string]any{"resourceType": "Bundle", "type": "searchset", "entry": entries}
	if off+size < len(match) && q.Get("_id") == "" {
		nq := r.URL.Query()
		nq.Set("_offset", strconv.Itoa(off+size))
		link := f.base() + "/" + typ + "?" + nq.Encode()
		if next != "" {
			link = next
		}
		b["link"] = []map[string]any{{"relation": "self", "url": "x"}, {"relation": "next", "url": link}}
	}
	writeJSON(w, b)
}

func lastUpdated(r map[string]any) string {
	m, _ := r["meta"].(map[string]any)
	s, _ := m["lastUpdated"].(string)
	return s
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/fhir+json")
	_ = json.NewEncoder(w).Encode(v)
}

// capability is a CapabilityStatement: version, and per resource type the
// search parameters it declares (nil: the full set for every type).
func capability(version string, params map[string][]string) map[string]any {
	types := []string{"Patient", "Observation", "MedicationRequest", "MedicationStatement", "Encounter", "Location"}
	var res []map[string]any
	for _, ty := range types {
		ps := []string{"_lastUpdated", "_sort", "_id"}
		if params != nil {
			p, ok := params[ty]
			if !ok {
				continue
			}
			ps = p
		}
		var sp []map[string]any
		for _, p := range ps {
			sp = append(sp, map[string]any{"name": p, "type": "date"})
		}
		res = append(res, map[string]any{"type": ty, "searchParam": sp})
	}
	return map[string]any{"resourceType": "CapabilityStatement", "fhirVersion": version,
		"rest": []map[string]any{{"mode": "server", "resource": res}}}
}

// verifyJWT checks a compact JWS's RS384 or ES384 signature with pub.
func verifyJWT(tok string, pub crypto.PublicKey) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	h := sha512.Sum384([]byte(parts[0] + "." + parts[1]))
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(k, crypto.SHA384, h[:], sig) == nil
	case *ecdsa.PublicKey:
		if len(sig) != 96 {
			return false
		}
		return ecdsa.Verify(k, h[:], new(big.Int).SetBytes(sig[:48]), new(big.Int).SetBytes(sig[48:]))
	}
	return false
}

// credsWith holds a PEM private key as the connection's credential.
func credsWith(pem []byte) connector.Creds {
	return connector.NewCreds(map[string]seal.Secret{"private_key": seal.NewSecret(pem)})
}

// memSink records what Fetch puts; failAfter > 0 refuses the put after
// that many.
type memSink struct {
	mu        sync.Mutex
	recs      []connector.RawRecord
	failAfter int
}

var errSinkFull = &connector.FetchError{Code: "unreachable"}

func (s *memSink) Put(_ context.Context, r connector.RawRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAfter > 0 && len(s.recs) >= s.failAfter {
		return errSinkFull
	}
	s.recs = append(s.recs, r)
	return nil
}

func (s *memSink) ids(kind string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.recs {
		if r.Kind == kind {
			out = append(out, r.SourceID)
		}
	}
	return out
}
