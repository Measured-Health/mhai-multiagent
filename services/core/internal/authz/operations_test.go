package authz

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

var nameRE = regexp.MustCompile(`^[a-z0-9_]+$`)

func TestOperationNamesMatchSubjects(t *testing.T) {
	ops := Operations()
	if len(ops) == 0 {
		t.Fatal("empty operations table")
	}
	for _, o := range ops {
		if !nameRE.MatchString(o.Service) || !nameRE.MatchString(o.Op) {
			t.Errorf("%s: service %q and op %q must be [a-z0-9_]+", o.Name, o.Service, o.Op)
		}
		if o.Name != o.Service+"."+o.Op {
			t.Errorf("name %q is not service.op (%q.%q)", o.Name, o.Service, o.Op)
		}
		switch o.Kind {
		case reqtoken.KindReq:
			if o.Timeout <= 0 || o.Timeout > reqtoken.MaxReqTTL {
				t.Errorf("%s: req timeout %v outside (0, %v]", o.Name, o.Timeout, reqtoken.MaxReqTTL)
			}
		case reqtoken.KindJob:
			if o.Timeout <= 0 || o.Timeout > reqtoken.MaxJobTTL {
				t.Errorf("%s: job timeout %v outside (0, %v]", o.Name, o.Timeout, reqtoken.MaxJobTTL)
			}
		default:
			t.Errorf("%s: kind %q is neither req nor job", o.Name, o.Kind)
		}
		for _, r := range o.AnyRole {
			if !KnownRole(r) || IsPlatformRole(r) {
				t.Errorf("%s: %q is not a clinical role", o.Name, r)
			}
		}
	}
	if !slices.IsSortedFunc(ops, func(a, b Operation) int { return strings.Compare(a.Name, b.Name) }) {
		t.Error("Operations() is not sorted by name")
	}
}

func TestOperationsFirstRows(t *testing.T) {
	rq, ok := OperationByName("pipeline.run_query")
	if !ok || rq.Kind != reqtoken.KindJob || !slices.Equal(rq.AnyRole, []string{"clinician"}) ||
		rq.SvcCore || !rq.NeedsHospital || rq.BFFDirect || rq.TestOnly || rq.Timeout != 10*time.Minute {
		t.Errorf("pipeline.run_query: %+v", rq)
	}
	h, ok := OperationByName("pipeline.health")
	if !ok || h.Kind != reqtoken.KindReq || len(h.AnyRole) != 0 || !h.SvcCore || h.NeedsHospital ||
		h.BFFDirect || h.TestOnly || h.Timeout != 2*time.Second {
		t.Errorf("pipeline.health: %+v", h)
	}
	e, ok := OperationByName("echo.ping")
	if !ok || e.Kind != reqtoken.KindReq || !slices.Equal(e.AnyRole, []string{"clinician"}) || !e.SvcCore ||
		!e.NeedsHospital || !e.BFFDirect || !e.TestOnly || e.Timeout != 5*time.Second {
		t.Errorf("echo.ping: %+v", e)
	}
	if _, ok := OperationByName("nope.nothing"); ok {
		t.Error("unknown operation found")
	}
}

func mustPanic(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected a panic")
		}
	}()
	f()
}

func TestRouteCallsUnknownOperationPanics(t *testing.T) {
	mustPanic(t, func() {
		Route(NewMux(nil), http.MethodPost, "/x", Rule{AnyRole: []string{RoleClinician}, Calls: []string{"pipeline.nope"}},
			http.NotFoundHandler())
	})
}

func TestRouteRolesSubsetOfOperation(t *testing.T) {
	h := http.NotFoundHandler()
	// A23: analyst may not call run_query.
	mustPanic(t, func() {
		Route(NewMux(nil), http.MethodPost, "/x", Rule{AnyRole: []string{RoleClinician, RoleAnalyst}, Calls: []string{"pipeline.run_query"}}, h)
	})
	// No AnyRole means every clinical role.
	mustPanic(t, func() {
		Route(NewMux(nil), http.MethodPost, "/x", Rule{Calls: []string{"pipeline.run_query"}}, h)
	})
	// An operation with no roles is callable by no route.
	mustPanic(t, func() {
		Route(NewMux(nil), http.MethodPost, "/x", Rule{AnyRole: []string{RoleClinician}, Calls: []string{"pipeline.health"}}, h)
	})
	// Public and platform routes call no operation.
	mustPanic(t, func() {
		Route(NewMux(nil), http.MethodPost, "/x", Rule{Public: true, Calls: []string{"pipeline.run_query"}}, h)
	})
	mustPanic(t, func() {
		Route(NewMux(nil), http.MethodPost, "/x", Rule{Platform: true, Write: true, AnyRole: []string{RolePlatformAdmin}, Calls: []string{"pipeline.run_query"}}, h)
	})
	// The subset passes.
	Route(NewMux(nil), http.MethodPost, "/x", Rule{AnyRole: []string{RoleClinician}, Calls: []string{"pipeline.run_query"}}, h)
}

func TestBFFDirectOnlyRequestReply(t *testing.T) {
	ok := Operation{Name: "a.b", Service: "a", Op: "b", Kind: reqtoken.KindReq, AnyRole: []string{RoleClinician},
		BFFDirect: true, Timeout: time.Second}
	if err := validateOperation(ok); err != nil {
		t.Fatalf("valid bff_direct operation refused: %v", err)
	}
	job := ok
	job.Kind, job.Timeout = reqtoken.KindJob, time.Minute
	if validateOperation(job) == nil {
		t.Error("a job marked bff_direct passed validation")
	}
	noRole := ok
	noRole.AnyRole = nil
	if validateOperation(noRole) == nil {
		t.Error("a role-less operation marked bff_direct passed validation")
	}
}

// Remove this test with the review that opens the first production
// bff_direct operation (spec §17.2).
func TestNoProductionBFFDirect(t *testing.T) {
	for _, o := range Operations() {
		if o.BFFDirect && !o.TestOnly {
			t.Errorf("%s is bff_direct but not test-only", o.Name)
		}
		if o.BFFDirect && o.Name != "echo.ping" {
			t.Errorf("only echo.ping may be bff_direct in this plan, found %s", o.Name)
		}
	}
}

func TestOperationsJSON(t *testing.T) {
	b, err := OperationsJSON(Operations())
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(Operations()) {
		t.Fatalf("rows %d, operations %d", len(rows), len(Operations()))
	}
	want := []string{"any_role", "bff_direct", "name", "needs_hospital", "op", "kind", "request_schema",
		"response_schema", "service", "svc_core", "test_only", "timeout_ms"}
	for _, r := range rows {
		for _, k := range want {
			if _, ok := r[k]; !ok {
				t.Errorf("%v: missing key %s", r["name"], k)
			}
		}
		if len(r) != len(want) {
			t.Errorf("%v: unexpected keys: %v", r["name"], r)
		}
		if _, ok := r["any_role"].([]any); !ok {
			t.Errorf("%v: any_role must be an array, not null", r["name"])
		}
	}
	var echo map[string]any
	for _, r := range rows {
		if r["name"] == "echo.ping" {
			echo = r
		}
	}
	if echo["test_only"] != true || echo["bff_direct"] != true || echo["timeout_ms"] != float64(5000) {
		t.Errorf("echo.ping row: %v", echo)
	}
}

func TestErrorCodesAreTheClosedSet(t *testing.T) {
	var got []string
	for _, c := range ErrorCodes() {
		if c.Meaning == "" {
			t.Errorf("%s has no meaning", c.Code)
		}
		got = append(got, c.Code)
	}
	want := []string{"unauthorized", "forbidden", "invalid_request", "not_found", "deadline_exceeded", "overloaded", "unavailable", "cancelled", "internal"}
	if !slices.Equal(got, want) {
		t.Errorf("error codes %v, want %v (spec 10.1)", got, want)
	}
}
