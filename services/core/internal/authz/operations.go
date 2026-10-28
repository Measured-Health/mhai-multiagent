package authz

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/reqtoken"
)

// Operation is one row of the operations table: a call that core may make to
// an intelligence worker over NATS (spec §5, §17.2). It is the only place
// the roles allowed to make each call are written. permgen publishes it as
// operations.json, which the Python workers read; Python writes no rule.
type Operation struct {
	// Name is Service + "." + Op, and the key in operations.json.
	Name    string
	Service string
	Op      string
	Kind    reqtoken.Kind
	// AnyRole: the caller must hold one of these clinical roles. Empty means
	// no user may call it (a service call with SvcCore only).
	AnyRole []string
	// SvcCore lets core call it as svc:core, with no user. Off by default.
	SvcCore bool
	// NeedsHospital: the body is scoped to the caller's hospital. False
	// means only "no body scope"; the token still carries a hid (F10).
	NeedsHospital bool
	// BFFDirect lets the BFF call it after a grant from core (spec §17.2).
	// Only request/reply operations with a role may set it.
	BFFDirect bool
	// TestOnly rows exist for the echo worker and are refused in production.
	TestOnly bool
	Timeout  time.Duration
	// RequestSchema and ResponseSchema are protobuf message names; empty
	// means none (a job has no reply message).
	RequestSchema  string
	ResponseSchema string
}

var operations = []Operation{
	{
		Name: "echo.ping", Service: "echo", Op: "ping", Kind: reqtoken.KindReq,
		AnyRole: []string{RoleClinician}, SvcCore: true, NeedsHospital: true, BFFDirect: true, TestOnly: true,
		Timeout:       5 * time.Second,
		RequestSchema: "mhai.intelligence.v1.PingRequest", ResponseSchema: "mhai.intelligence.v1.PingResponse",
	},
	{
		Name: "pipeline.health", Service: "pipeline", Op: "health", Kind: reqtoken.KindReq,
		SvcCore: true, Timeout: 2 * time.Second,
		RequestSchema: "mhai.intelligence.v1.HealthRequest", ResponseSchema: "mhai.intelligence.v1.HealthResponse",
	},
	{
		Name: "pipeline.run_query", Service: "pipeline", Op: "run_query", Kind: reqtoken.KindJob,
		AnyRole: []string{RoleClinician}, NeedsHospital: true, Timeout: 10 * time.Minute,
		RequestSchema: "mhai.intelligence.v1.RunQueryJob",
	},
}

func init() {
	slices.SortFunc(operations, func(a, b Operation) int { return strings.Compare(a.Name, b.Name) })
	seen := map[string]bool{}
	for _, o := range operations {
		if err := validateOperation(o); err != nil {
			panic(err)
		}
		if seen[o.Name] {
			panic(fmt.Errorf("authz: duplicate operation %q", o.Name))
		}
		seen[o.Name] = true
	}
}

func validateOperation(o Operation) error {
	if o.Name != o.Service+"."+o.Op || !operationPart(o.Service) || !operationPart(o.Op) {
		return fmt.Errorf("authz: operation %q must be service.op with [a-z0-9_]+ parts", o.Name)
	}
	limit := time.Duration(0)
	switch o.Kind {
	case reqtoken.KindReq:
		limit = reqtoken.MaxReqTTL
	case reqtoken.KindJob:
		limit = reqtoken.MaxJobTTL
	default:
		return fmt.Errorf("authz: operation %s has kind %q, not req or job", o.Name, o.Kind)
	}
	if o.Timeout <= 0 || o.Timeout > limit {
		return fmt.Errorf("authz: operation %s timeout %v is outside (0, %v]", o.Name, o.Timeout, limit)
	}
	for _, r := range o.AnyRole {
		if !KnownRole(r) || IsPlatformRole(r) {
			return fmt.Errorf("authz: operation %s names %q, which is not a clinical role", o.Name, r)
		}
	}
	if o.BFFDirect && (o.Kind != reqtoken.KindReq || len(o.AnyRole) == 0) {
		return fmt.Errorf("authz: operation %s: bff_direct needs a request/reply operation with at least one role", o.Name)
	}
	return nil
}

func operationPart(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Operations returns the table, sorted by name. The caller gets a copy.
func Operations() []Operation {
	out := make([]Operation, len(operations))
	for i, o := range operations {
		o.AnyRole = slices.Clone(o.AnyRole)
		out[i] = o
	}
	return out
}

// OperationByName returns the named operation.
func OperationByName(name string) (Operation, bool) {
	for _, o := range Operations() {
		if o.Name == name {
			return o, true
		}
	}
	return Operation{}, false
}

// clinicalRoles are the roles a route with no AnyRole admits.
var clinicalRoles = []string{RoleClinician, RoleHospitalAdmin, RoleAnalyst}

// validateCalls checks the operations a route publishes to: each exists, and
// every role the route admits is a role the operation allows.
func validateCalls(method, pattern string, rule Rule) error {
	if len(rule.Calls) == 0 {
		return nil
	}
	if rule.Public || rule.Platform || rule.SelfService {
		return fmt.Errorf("authz: route %s %s is public, platform or self-service and cannot call an operation", method, pattern)
	}
	admitted := SortRoles(rule.AnyRole)
	if len(admitted) == 0 {
		admitted = clinicalRoles
	}
	for _, name := range rule.Calls {
		op, ok := OperationByName(name)
		if !ok {
			return fmt.Errorf("authz: route %s %s calls unknown operation %q", method, pattern, name)
		}
		for _, r := range admitted {
			if !slices.Contains(op.AnyRole, r) {
				return fmt.Errorf("authz: route %s %s admits role %q, which operation %s does not allow", method, pattern, r, name)
			}
		}
	}
	return nil
}

type jsonOperation struct {
	Name           string   `json:"name"`
	Service        string   `json:"service"`
	Op             string   `json:"op"`
	Kind           string   `json:"kind"`
	AnyRole        []string `json:"any_role"`
	SvcCore        bool     `json:"svc_core"`
	NeedsHospital  bool     `json:"needs_hospital"`
	BFFDirect      bool     `json:"bff_direct"`
	TimeoutMS      int64    `json:"timeout_ms"`
	RequestSchema  string   `json:"request_schema"`
	ResponseSchema string   `json:"response_schema"`
	TestOnly       bool     `json:"test_only"`
}

// OperationsJSON renders ops as operations.json, sorted by name.
func OperationsJSON(ops []Operation) ([]byte, error) {
	sorted := slices.Clone(ops)
	slices.SortFunc(sorted, func(a, b Operation) int { return strings.Compare(a.Name, b.Name) })
	rows := make([]jsonOperation, 0, len(sorted))
	for _, o := range sorted {
		roles := slices.Clone(o.AnyRole)
		if roles == nil {
			roles = []string{}
		}
		rows = append(rows, jsonOperation{
			Name: o.Name, Service: o.Service, Op: o.Op, Kind: string(o.Kind), AnyRole: roles,
			SvcCore: o.SvcCore, NeedsHospital: o.NeedsHospital, BFFDirect: o.BFFDirect,
			TimeoutMS: o.Timeout.Milliseconds(), RequestSchema: o.RequestSchema,
			ResponseSchema: o.ResponseSchema, TestOnly: o.TestOnly,
		})
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
