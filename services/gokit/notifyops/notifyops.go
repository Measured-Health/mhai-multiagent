// Package notifyops is the notifications service's control-operations
// table (ADR 0018; notifications design §7): who may call each
// notify.v1.ctl.<op>, with what timeout, and its request and response
// messages. Core's client and the notifications responder both read it, so
// the rule is written once. It is not core's operations table and is never
// published to Python.
//
// No op admits a user, a platform role or an auditor: every op is
// svc:core's alone. PR 2 built health and PR 5 send_link (hospital-bound:
// the token's hid is the recipient's hospital); send_platform_link is a
// platform team member's invite (no hospital); unsubscribe_check comes
// with PR 7.
package notifyops

import (
	"strings"
	"time"
)

// SubjectPrefix is the control subjects' prefix (design §7).
const SubjectPrefix = "notify.v1.ctl."

// svcCore is the request token's sub for work no user started
// (reqtoken.SvcCore).
const svcCore = "svc:core"

// Op is one control operation.
type Op struct {
	Name string // "notifications." + Op
	Op   string // the subject's last segment
	// SvcCore: the token's sub is svc:core, with no roles. An op with no
	// caller rule allows nobody.
	SvcCore bool
	// HospitalBound: the op acts for one hospital (the token's hid), so a
	// token without a valid hid is refused before the handler runs.
	HospitalBound bool
	Timeout       time.Duration
	// Request and Response are the protobuf full names (mhai.notify.v1.*).
	Request, Response string
}

// hospitalBound marks o as acting for the token's hospital.
func hospitalBound(o Op) Op {
	o.HospitalBound = true
	return o
}

func coreOp(op string, timeout time.Duration, msg string) Op {
	return Op{Name: "notifications." + op, Op: op, SvcCore: true, Timeout: timeout,
		Request: "mhai.notify.v1." + msg + "Request", Response: "mhai.notify.v1." + msg + "Reply"}
}

// Ops is the table.
var Ops = []Op{
	coreOp("health", 5*time.Second, "Health"),
	// An invite or a reset, sent at once with its link in memory: up to 3
	// tries in about 25 seconds (design §4.4).
	hospitalBound(coreOp("send_link", 30*time.Second, "SendLink")),
	// A platform team member's invite: the same message, for an account
	// with no hospital, so the token names none (reqtoken's
	// PlatformLinkSubject) and the handler reads no hospital's rows.
	coreOp("send_platform_link", 30*time.Second, "SendLink"),
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
	}
	return false
}
