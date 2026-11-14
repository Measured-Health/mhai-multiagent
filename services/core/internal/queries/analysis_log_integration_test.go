//go:build integration

package queries_test

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/telemetry"
)

// TestAnalysisQueryNotLogged (analysis plan N5): an analysis request's
// filter and period values never reach core's logs or span attributes. No
// analysis route exists yet; this pins today's behaviour so a later access
// log or route can't add the query string.
func TestAnalysisQueryNotLogged(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	shutdown, err := telemetry.SetupWith(exp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	e := newEnv(t, 10)
	hid := seedHospital(t)
	u := seedUser(t, hid, authz.RoleHospitalAdmin)
	access := mint(t, u)
	const cond, start = "ZZSYNTHCOND9", "2031-07-22"
	for _, path := range []string{
		"/analysis/prevalence?condition=" + cond + "&period=month&start=" + start,
		"/analysis/patients?drill=" + cond + "&level=deidentified&period=week&start=" + start,
	} {
		if r := e.do(t, "GET", path, access, "", nil); r.code < 400 {
			t.Fatalf("%s: %d", path, r.code)
		}
	}
	if e.do(t, "GET", "/analysis/prevalence?condition="+cond, "", "", nil).code < 400 {
		t.Fatal("an unauthenticated analysis request was answered")
	}
	for _, s := range exp.GetSpans() {
		texts := []string{s.Name, s.Status.Description}
		for _, kv := range s.Attributes {
			texts = append(texts, kv.Value.String())
		}
		for _, ev := range s.Events {
			for _, kv := range ev.Attributes {
				texts = append(texts, kv.Value.String())
			}
		}
		for _, text := range texts {
			if strings.Contains(text, cond) || strings.Contains(text, start) {
				t.Fatalf("a query value reached span %q: %q", s.Name, text)
			}
		}
	}
	if logs := e.logs.String(); strings.Contains(logs, cond) || strings.Contains(logs, start) {
		t.Fatalf("a query value reached the logs: %s", logs)
	}
}
