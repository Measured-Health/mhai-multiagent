package notifyops

import (
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/Measured-Health/mhai-multiagent/services/gokit/notifypb"
)

const user = "11111111-1111-4111-8111-111111111111"

// The table (notifications design §7; §15 PR 2): health only, called as
// svc:core. Each later op is added with its PR.
func TestOpsTable(t *testing.T) {
	if len(Ops) != 1 {
		t.Fatalf("%d ops, want 1", len(Ops))
	}
	o := Ops[0]
	if o.Op != "health" || o.Name != "notifications.health" || !o.SvcCore || o.HospitalBound || o.Timeout != 5*time.Second {
		t.Fatalf("%+v", o)
	}
	for _, name := range []string{o.Request, o.Response} {
		if _, err := protoregistry.GlobalTypes.FindMessageByName(protoreflect.FullName(name)); err != nil {
			t.Errorf("no message %s", name)
		}
	}
	if got, ok := Lookup(Subject("health")); !ok || got.Name != o.Name {
		t.Fatalf("Lookup: %+v %v", got, ok)
	}
	if Subject("health") != "notify.v1.ctl.health" {
		t.Fatal(Subject("health"))
	}
	for _, s := range []string{"", "notify.v1.ctl", "notify.v1.ctl.nope", "notify.v1.ctl.health.x",
		"inference.v1.notifications.health", "notify.v1.ctl.HEALTH", "notify.v2.ctl.health", "emr.v1.ctl.health",
		"documents.v1.ctl.health", "notify.v1.ctl.send_link", "notify.v1.ctl.unsubscribe_check"} {
		if _, ok := Lookup(s); ok {
			t.Errorf("Lookup(%q) found an op", s)
		}
	}
}

// No op admits a user, a platform role or an auditor: health is svc:core's
// alone, and svc:core carries no roles.
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
		{Op{Name: "notifications.x", Op: "x"}, "svc:core", nil, false}, // no caller rule: nobody
	} {
		if got := tc.op.Allows(tc.sub, tc.roles); got != tc.ok {
			t.Errorf("%s by %q %v: %v, want %v", tc.op.Name, tc.sub, tc.roles, got, tc.ok)
		}
	}
}
