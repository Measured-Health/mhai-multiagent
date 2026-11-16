// Package docops is the documents service's control-operations table
// (ADR 0016; documents design §8, §17 departure 5): who may call each
// documents.v1.ctl.<op>, with what timeout, and its request and response
// messages. Core's client (PR 5) and the documents responder both read it,
// so the rule is written once. It is not core's operations table and is
// never published to Python.
//
// No op allows a platform role or an auditor (S19): platform roles hold
// nothing on documents. PR 1 built health and PR 3 presign_upload, both as
// svc:core; each later op comes with the PR that builds it.
//
// The control subject carries no hospital segment, so an op that reads a
// row is HospitalBound: the responder refuses its token unless the hid is
// a hospital id, and the handler reads the row under that hospital only
// (row-level security), so another hospital's id is not_found (DS1;
// design §17 departure 10).
package docops

import (
	"strings"
	"time"
)

// SubjectPrefix is the control subjects' prefix (documents design §8).
const SubjectPrefix = "documents.v1.ctl."

// svcCore is the request token's sub for work no user started
// (reqtoken.SvcCore).
const svcCore = "svc:core"

// Op is one control operation.
type Op struct {
	Name string // "documents." + Op
	Op   string // the subject's last segment
	// SvcCore: the token's sub is svc:core, with no roles. An op with no
	// caller rule allows nobody.
	SvcCore bool
	// HospitalBound: the op reads a row of the token's hospital (hid), so
	// a token without a valid hid is refused before the handler runs.
	HospitalBound bool
	Timeout       time.Duration
	// Request and Response are the protobuf full names
	// (mhai.documents.v1.*).
	Request, Response string
}

func coreOp(op string, timeout time.Duration, msg string) Op {
	return Op{Name: "documents." + op, Op: op, SvcCore: true, Timeout: timeout,
		Request: "mhai.documents.v1." + msg + "Request", Response: "mhai.documents.v1." + msg + "Reply"}
}

// hospitalBound marks o as reading a row of the token's hospital.
func hospitalBound(o Op) Op {
	o.HospitalBound = true
	return o
}

// Ops is the table.
var Ops = []Op{
	coreOp("health", 5*time.Second, "Health"),
	// The POST policy for a row's quarantine key (design §4.2).
	hospitalBound(coreOp("presign_upload", 5*time.Second, "PresignUpload")),
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
