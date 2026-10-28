//go:build integration && test

package natsapi_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/httpx"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/natsapi"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/server"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

func newIP() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("10.%d.%d.%d", b[0], b[1], b[2]%250+1)
}

// shape is v with every value replaced by its JSON type, so two 2xx bodies
// that differ only in ids and times compare equal.
func shape(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[k] = shape(vv)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = shape(x[i])
		}
		return out
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// same fails unless the HTTP and NATS answers have the same status, the
// same body (a problem body minus its request_id; a 2xx body by shape)
// and the same allow-listed headers (X-Request-ID by presence, since each
// request gets its own).
func same(t *testing.T, name string, h, n resp) {
	t.Helper()
	if h.code != n.code {
		t.Errorf("%s: status HTTP %d %s, NATS %d %s", name, h.code, h.raw, n.code, n.raw)
		return
	}
	for _, k := range natsapi.ResponseHeaders {
		hv, nv := h.hdr.Values(k), n.hdr.Values(k)
		if k == "X-Request-ID" {
			if len(hv) != 1 || len(nv) != 1 {
				t.Errorf("%s: X-Request-ID HTTP %v, NATS %v", name, hv, nv)
			}
			continue
		}
		if !reflect.DeepEqual(hv, nv) {
			t.Errorf("%s: header %s HTTP %v, NATS %v", name, k, hv, nv)
		}
	}
	if n.body != nil {
		if rid, ok := n.body["request_id"]; ok && rid != n.hdr.Get("X-Request-ID") {
			t.Errorf("%s: NATS problem request_id %v, header %q", name, rid, n.hdr.Get("X-Request-ID"))
		}
	}
	switch {
	case h.body == nil || n.body == nil:
		if h.raw != n.raw {
			t.Errorf("%s: body HTTP %q, NATS %q", name, h.raw, n.raw)
		}
	case h.code < 300:
		if !reflect.DeepEqual(shape(h.body), shape(n.body)) {
			t.Errorf("%s: body shape HTTP %s, NATS %s", name, h.raw, n.raw)
		}
	default:
		hb, nb := clone(h.body), clone(n.body)
		delete(hb, "request_id")
		delete(nb, "request_id")
		if !reflect.DeepEqual(hb, nb) {
			t.Errorf("%s: body HTTP %s, NATS %s", name, h.raw, n.raw)
		}
	}
}

func clone(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestAPIParityOverNATS (A38, review focus 7): every route of server.Table()
// gives the same answer over api.v1.<audience> as over HTTP, for the Plan 2
// auth fixture matrix: no token, the right and the wrong audience, a
// hospital admin on a clinician route, an auditor, a deactivated user, and
// another hospital's row (every {param} is a row of another hospital). Each
// request gets a fresh account and session, so nothing one call changes
// (logout, revoke-all) reaches another. The probes are HTTP only: over NATS
// they are 404.
func TestAPIParityOverNATS(t *testing.T) {
	e := newEnv(t)
	e.core(t, nil)
	ctx := context.Background()
	hidA, hidB := seedHospital(t), seedHospital(t)
	rowB := uuid.New()
	if _, err := super.Exec(ctx, `INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency)
		VALUES ($1, $2, 'Synthetic question', 'routine')`, rowB, hidB); err != nil {
		t.Fatal(err)
	}
	clinical := func(roles ...string) func() string {
		return func() string { return mint(t, seedUser(t, hidA, true, roles...)) }
	}
	platform := func(role string) func() string {
		return func() string { return mint(t, seedUser(t, uuid.Nil, true, role)) }
	}
	none := func() string { return "" }
	cases := []struct {
		name, app string
		tok       func() string
	}{
		{"no token", natsapi.AppClinical, none},
		{"no token, platform", natsapi.AppPlatform, none},
		{"clinician", natsapi.AppClinical, clinical(authz.RoleClinician)},
		{"clinician, platform audience", natsapi.AppPlatform, clinical(authz.RoleClinician)},
		{"hospital admin", natsapi.AppClinical, clinical(authz.RoleHospitalAdmin)},
		{"platform admin", natsapi.AppPlatform, platform(authz.RolePlatformAdmin)},
		{"platform admin, clinical audience", natsapi.AppClinical, platform(authz.RolePlatformAdmin)},
		{"auditor", natsapi.AppPlatform, platform(authz.RoleAuditor)},
		{"deactivated clinician", natsapi.AppClinical, func() string {
			return mint(t, seedUser(t, hidA, false, authz.RoleClinician))
		}},
	}
	table := server.Table()
	if len(table) < 10 {
		t.Fatalf("table has %d routes", len(table))
	}
	compared := 0
	for _, rr := range table {
		path := pathParam.ReplaceAllString(strings.TrimPrefix(rr.Pattern, httpx.BasePath), rowB.String())
		route := rr.Method + " " + rr.Pattern
		if rr.Rule.Probe {
			n := e.mustNATS(t, call{method: rr.Method, path: path})
			if n.code != 404 || n.body["code"] != "not_found" {
				t.Errorf("%s over NATS: %d %s", route, n.code, n.raw)
			}
			if h := e.http(t, call{method: rr.Method, path: path}); h.code == 404 {
				t.Errorf("%s over HTTP: 404", route)
			}
			continue
		}
		for _, c := range cases {
			base := call{app: c.app, method: rr.Method, path: path}
			if rr.Method != http.MethodGet && rr.Method != http.MethodHead {
				base.body = "{}"
			}
			h, n := base, base
			h.bearer, h.ip = c.tok(), newIP()
			n.bearer, n.ip = c.tok(), newIP()
			same(t, route+" / "+c.name, e.http(t, h), e.mustNATS(t, n))
			compared++
		}
	}
	t.Logf("%d routes, %d comparisons", len(table), compared)
}

// Idempotent replay and conflict give the same answers on both paths, and
// a replay over NATS doesn't run the write again.
func TestIdempotencyParityOverNATS(t *testing.T) {
	e := newEnv(t)
	e.core(t, nil)
	hid := seedHospital(t)
	type seq [3]resp
	run := func(do func(call) resp) (seq, string) {
		tok := mint(t, seedUser(t, hid, true, authz.RoleClinician))
		key := "key-" + uuid.NewString()
		c := call{method: "POST", path: "/_natsapi/write", bearer: tok, hdr: map[string]string{"Idempotency-Key": key}}
		var s seq
		c.body = `{"n":1}`
		s[0] = do(c)
		s[1] = do(c)
		c.body = `{"n":2}`
		s[2] = do(c)
		return s, key
	}
	hs, hk := run(func(c call) resp { return e.http(t, c) })
	ns, nk := run(func(c call) resp { return e.mustNATS(t, c) })
	for i, name := range []string{"first", "replay", "conflict"} {
		same(t, name, hs[i], ns[i])
	}
	if ns[0].code != 200 || ns[1].code != 200 || ns[1].hdr.Get("Idempotent-Replayed") != "true" || ns[2].code != 409 {
		t.Fatalf("NATS sequence %d, %d (%q), %d", ns[0].code, ns[1].code, ns[1].hdr.Get("Idempotent-Replayed"), ns[2].code)
	}
	if e.writes.get(hk) != 1 || e.writes.get(nk) != 1 {
		t.Fatalf("writes ran HTTP %d, NATS %d times", e.writes.get(hk), e.writes.get(nk))
	}
}

// TestAudienceFromSubjectOnly (A38): the subject fixes the audience. On
// api.v1.clinical an X-MHAI-Audience: platform header changes nothing: a
// platform account's token gets 403 not_platform, as a clinical problem
// body; on api.v1.platform an X-MHAI-Audience: clinical header changes
// nothing either.
func TestAudienceFromSubjectOnly(t *testing.T) {
	e := newEnv(t)
	e.core(t, nil)
	pa := mint(t, seedUser(t, uuid.Nil, true, authz.RolePlatformAdmin))
	r := e.mustNATS(t, call{method: "GET", path: "/_test/platform", bearer: pa, hdr: map[string]string{"X-MHAI-Audience": "platform"}})
	if r.code != 403 || r.body["code"] != "not_platform" || r.hdr.Get("Content-Type") != "application/problem+json" {
		t.Fatalf("platform token on api.v1.clinical: %d %s", r.code, r.raw)
	}
	r = e.mustNATS(t, call{app: natsapi.AppPlatform, method: "GET", path: "/_test/platform", bearer: pa,
		hdr: map[string]string{"X-MHAI-Audience": "clinical"}})
	if r.code != 200 {
		t.Fatalf("platform token on api.v1.platform: %d %s", r.code, r.raw)
	}
	clin := mint(t, seedUser(t, seedHospital(t), true, authz.RoleClinician))
	r = e.mustNATS(t, call{method: "GET", path: "/_test/clinical", bearer: clin, hdr: map[string]string{"X-MHAI-Audience": "platform"}})
	if r.code != 200 {
		t.Fatalf("clinician on api.v1.clinical with a platform header: %d %s", r.code, r.raw)
	}
	r = e.mustNATS(t, call{app: natsapi.AppPlatform, method: "GET", path: "/_test/clinical", bearer: clin})
	if r.code != 403 || r.raw != `{"code":"not_platform"}` {
		t.Fatalf("clinician on api.v1.platform: %d %s", r.code, r.raw)
	}
}
