package emrops

import (
	"testing"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

const user = "11111111-1111-4111-8111-111111111111"

// The table is the design's (§3.2) with the plan's additions (departures
// 2 and 12): who may call each op, step-up, timeouts.
func TestOpsTable(t *testing.T) {
	want := map[string]struct {
		platform, svcCore, stepUp bool
		timeout                   time.Duration
	}{
		"test":             {true, false, false, 30 * time.Second},
		"put_credentials":  {true, false, true, 10 * time.Second},
		"wipe_credentials": {true, false, false, 10 * time.Second},
		"redrive":          {true, false, true, 10 * time.Second},
		"health":           {false, true, false, 5 * time.Second},
		"sync_now":         {false, true, false, 10 * time.Second},
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
		if o.Name != "emr."+o.Op || (len(o.AnyPlatformRole) > 0) != w.platform || o.SvcCore != w.svcCore ||
			o.StepUp != w.stepUp || o.Timeout != w.timeout {
			t.Errorf("%s: %+v", o.Op, o)
		}
		if o.SvcCore == (len(o.AnyPlatformRole) > 0) {
			t.Errorf("%s: exactly one of SvcCore and AnyPlatformRole", o.Op)
		}
		for _, r := range o.AnyPlatformRole {
			if r != "platform_admin" {
				t.Errorf("%s allows %s", o.Op, r)
			}
		}
		// Request and response are messages of the emr contract.
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
	if Subject("health") != "emr.v1.ctl.health" {
		t.Fatal(Subject("health"))
	}
	for _, s := range []string{"", "emr.v1.ctl", "emr.v1.ctl.nope", "emr.v1.ctl.health.x", "inference.v1.emr.health",
		"emr.v1.ctl.HEALTH", "emr.v2.ctl.health"} {
		if _, ok := Lookup(s); ok {
			t.Errorf("Lookup(%q) found an op", s)
		}
	}
}

// A4: the caller rule. svc:core calls only the svc:core ops; a platform
// admin (a user id) only the platform ops; nobody else anything.
func TestCallerRule(t *testing.T) {
	health, _ := Lookup(Subject("health"))
	put, _ := Lookup(Subject("put_credentials"))
	for _, tc := range []struct {
		op    Op
		sub   string
		roles []string
		ok    bool
	}{
		{health, "svc:core", nil, true},
		{health, "svc:core", []string{"platform_admin"}, false}, // svc:core carries no roles
		{health, user, []string{"platform_admin"}, false},
		{put, user, []string{"platform_admin"}, true},
		{put, user, []string{"auditor", "platform_admin"}, true},
		{put, "svc:core", nil, false},
		{put, user, []string{"clinician"}, false},
		{put, user, []string{"hospital_admin"}, false},
		{put, user, nil, false},
		{put, "not-a-uuid", []string{"platform_admin"}, false},
		{put, "", []string{"platform_admin"}, false},
		{Op{}, "svc:core", nil, false},
	} {
		if got := tc.op.Allows(tc.sub, tc.roles); got != tc.ok {
			t.Errorf("%s by %q %v: %v, want %v", tc.op.Name, tc.sub, tc.roles, got, tc.ok)
		}
	}
}
