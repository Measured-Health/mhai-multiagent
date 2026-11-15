package docops

import (
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/Measured-Health/mhai-multiagent/services/gokit/docspb"
)

const user = "11111111-1111-4111-8111-111111111111"

// The table (documents design §8; §17 departure 5): in PR 1 only health,
// called as svc:core with no step-up. Each later op is added with its PR.
func TestOpsTable(t *testing.T) {
	want := map[string]struct {
		svcCore bool
		timeout time.Duration
	}{
		"health": {true, 5 * time.Second},
	}
	if len(Ops) != len(want) {
		t.Fatalf("%d ops, want %d", len(Ops), len(want))
	}
	for _, o := range Ops {
		w, ok := want[o.Op]
		if !ok {
			t.Errorf("unexpected op %s", o.Op)
			continue
		}
		if o.Name != "documents."+o.Op || o.SvcCore != w.svcCore || o.Timeout != w.timeout {
			t.Errorf("%s: %+v", o.Op, o)
		}
		// Request and response are messages of the documents contract.
		for _, name := range []string{o.Request, o.Response} {
			if _, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(name)); err != nil {
				t.Errorf("%s: no message %s", o.Op, name)
			}
		}
		got, ok := Lookup(Subject(o.Op))
		if !ok || got.Name != o.Name {
			t.Errorf("Lookup(%s) = %+v, %v", Subject(o.Op), got, ok)
		}
	}
	if Subject("health") != "documents.v1.ctl.health" {
		t.Fatal(Subject("health"))
	}
	for _, s := range []string{"", "documents.v1.ctl", "documents.v1.ctl.nope", "documents.v1.ctl.health.x",
		"inference.v1.documents.health", "documents.v1.ctl.HEALTH", "documents.v2.ctl.health", "emr.v1.ctl.health",
		"documents.v1.ctl.presign_upload", "documents.v1.ctl.page_image"} {
		if _, ok := Lookup(s); ok {
			t.Errorf("Lookup(%q) found an op", s)
		}
	}
}

// S19 (DS11): no documents op admits a platform role, an auditor or any
// user in PR 1: health is svc:core's alone, and svc:core carries no roles.
func TestCallerRule(t *testing.T) {
	health, _ := Lookup(Subject("health"))
	for _, tc := range []struct {
		op    Op
		sub   string
		roles []string
		ok    bool
	}{
		{health, "svc:core", nil, true},
		{health, "svc:core", []string{}, true},
		{health, "svc:core", []string{"platform_admin"}, false},
		{health, user, []string{"platform_admin"}, false},
		{health, user, []string{"auditor"}, false},
		{health, user, []string{"hospital_admin"}, false},
		{health, user, []string{"clinician"}, false},
		{health, "svc:emr", nil, false},
		{health, "SVC:CORE", nil, false},
		{health, "", nil, false},
		{Op{}, "svc:core", nil, false},
		{Op{Name: "documents.x", Op: "x"}, "svc:core", nil, false}, // no caller rule: nobody
	} {
		if got := tc.op.Allows(tc.sub, tc.roles); got != tc.ok {
			t.Errorf("%s by %q %v: %v, want %v", tc.op.Name, tc.sub, tc.roles, got, tc.ok)
		}
	}
}
