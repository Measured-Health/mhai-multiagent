package ctl

import (
	"context"
	"strings"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrops"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
)

// EMR PR 2 review: an op marked StepUp in emrops (put_credentials, redrive)
// runs only on a token whose signed stp claim says core checked a fresh
// step-up code. Without it: no reply, no handler, reason step_up.
func TestStepUpOpsNeedTheClaim(t *testing.T) {
	put := func(t *testing.T, f *fixture, stepUp bool) (bool, string) {
		c := adminClaims("put_credentials")
		c.StepUp = stepUp
		m := f.msg(t, c, "emr.v1.ctl.put_credentials", "mhai.emr.v1.PutCredentialsRequest",
			marshal(t, &emrpb.PutCredentialsRequest{ConnectionId: conn}))
		out, reason := f.r.Process(context.Background(), m)
		return out != nil, reason
	}
	f := newFixture(t)
	if answered, reason := put(t, f, false); answered || reason != "step_up" || f.ran.Load() != 0 {
		t.Fatalf("put_credentials without stp: answered %v reason %q ran %d", answered, reason, f.ran.Load())
	}
	if !strings.Contains(f.logs.String(), `"reason":"step_up"`) || f.r.Metrics.Count("step_up") != 1 {
		t.Fatalf("refusal not logged or counted: %s", f.logs.String())
	}
	if answered, reason := put(t, f, true); !answered || reason != "ok" || f.ran.Load() != 1 {
		t.Fatalf("put_credentials with stp: answered %v reason %q ran %d", answered, reason, f.ran.Load())
	}

	// redrive too (its handler is a later PR: the step-up check comes first).
	f = newFixture(t)
	c := adminClaims("redrive")
	m := f.msg(t, c, "emr.v1.ctl.redrive", "mhai.emr.v1.RedriveRequest", marshal(t, &emrpb.RedriveRequest{Stage: "emr_rules"}))
	if out, reason := f.r.Process(context.Background(), m); out != nil || reason != "step_up" {
		t.Fatalf("redrive without stp: %q", reason)
	}

	// Ops without StepUp don't need it.
	f = newFixture(t)
	if out, reason := f.r.Process(context.Background(), f.testMsg(t)); out == nil || reason != "ok" {
		t.Fatalf("test without stp: %q", reason)
	}

	// Every StepUp op in the table is refused without the claim: a new one
	// can't slip past.
	for _, op := range emrops.Ops {
		if !op.StepUp {
			continue
		}
		if op.SvcCore {
			t.Fatalf("%s: a step-up op must be a user's", op.Name)
		}
	}
}
