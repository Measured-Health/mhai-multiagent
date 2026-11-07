// Package emrops is the emr service's control-operations table (plan
// departure 2): who may call each emr.v1.ctl.<op>, with what step-up and
// timeout, and its request and response messages. Core's emrctl client
// and the emr responder both read it, so the rule is written once. It is
// not core's operations table and is never published to Python: those
// rows allow clinical roles only, and these allow a platform admin or
// svc:core.
package emrops

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SubjectPrefix is the control subjects' prefix (EMR design §3.2).
const SubjectPrefix = "emr.v1.ctl."

// svcCore is the request token's sub for work no user started
// (reqtoken.SvcCore).
const svcCore = "svc:core"

// PlatformAdmin is the role a platform op allows.
const PlatformAdmin = "platform_admin"

// Op is one control operation.
type Op struct {
	Name string // "emr." + Op
	Op   string // the subject's last segment
	// AnyPlatformRole: the token's sub is a user id holding one of these.
	AnyPlatformRole []string
	// SvcCore: the token's sub is svc:core (no roles).
	SvcCore bool
	// StepUp: core checks a fresh step-up code before it calls.
	StepUp  bool
	Timeout time.Duration
	// Request and Response are the protobuf full names (mhai.emr.v1.*).
	Request, Response string
}

func platformOp(op string, stepUp bool, timeout time.Duration, msg string) Op {
	return Op{Name: "emr." + op, Op: op, AnyPlatformRole: []string{PlatformAdmin}, StepUp: stepUp, Timeout: timeout,
		Request: "mhai.emr.v1." + msg + "Request", Response: "mhai.emr.v1." + msg + "Reply"}
}

func coreOp(op string, timeout time.Duration, msg string) Op {
	return Op{Name: "emr." + op, Op: op, SvcCore: true, Timeout: timeout,
		Request: "mhai.emr.v1." + msg + "Request", Response: "mhai.emr.v1." + msg + "Reply"}
}

// Ops is the table.
var Ops = []Op{
	platformOp("test", false, 30*time.Second, "Test"),
	platformOp("put_credentials", true, 10*time.Second, "PutCredentials"),
	platformOp("wipe_credentials", false, 10*time.Second, "WipeCredentials"),
	platformOp("redrive", true, 10*time.Second, "Redrive"),
	coreOp("health", 5*time.Second, "Health"),
	coreOp("sync_now", 10*time.Second, "SyncNow"),
}

// Subject is op's control subject.
func Subject(op string) string { return SubjectPrefix + op }

// Lookup returns the op subject names, exactly.
func Lookup(subject string) (Op, bool) {
	name, ok := strings.CutPrefix(subject, SubjectPrefix)
	if !ok {
		return Op{}, false
	}
	for _, o := range Ops {
		if o.Op == name {
			return o, true
		}
	}
	return Op{}, false
}

// Allows is the op's caller rule, on a verified token's sub and roles.
func (o Op) Allows(sub string, roles []string) bool {
	switch {
	case o.Name == "":
		return false
	case o.SvcCore:
		return sub == svcCore && len(roles) == 0
	case len(o.AnyPlatformRole) > 0:
		if _, err := uuid.Parse(sub); err != nil || len(sub) != 36 {
			return false
		}
		for _, r := range roles {
			if slices.Contains(o.AnyPlatformRole, r) {
				return true
			}
		}
	}
	return false
}
