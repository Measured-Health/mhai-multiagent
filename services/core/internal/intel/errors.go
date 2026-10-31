package intel

import (
	"errors"
	"fmt"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
)

// Errors core itself returns. Each maps to one problem code in Code; none
// carries a body, a token, a claim or a driver message.
var (
	// ErrTimeout: no reply before the call's deadline (NATS timeout), or
	// too little time left to send it at all.
	ErrTimeout = errors.New("intel: no reply in time")
	// ErrNoResponders: no worker is subscribed (NATS 503).
	ErrNoResponders = errors.New("intel: no worker running")
	// ErrBackpressure: JOBS refused the publish (its per-hospital subject,
	// or the stream, is full: DiscardNewPerSubject, F3).
	ErrBackpressure = errors.New("intel: the job queue refused the job")
	// ErrPublish: the job couldn't be published for another reason.
	ErrPublish = errors.New("intel: job not published")
	// ErrAtCap: the hospital already has the limit of questions queued or
	// running (decision 8).
	ErrAtCap = errors.New("intel: too many questions in flight for this hospital")
	// ErrNotAllowed: core's own check of the operation's roles refused the
	// caller; nothing was sent.
	ErrNotAllowed = errors.New("intel: the caller may not call this operation")
	// ErrBadReply: a reply that doesn't hold together (wrong schema, a
	// request id that isn't ours, an undecodable body, an unknown code).
	ErrBadReply = errors.New("intel: malformed reply")
	// ErrDeadline: a job whose deadline is past (or under a second away).
	ErrDeadline = errors.New("intel: the job's deadline has passed")
	// ErrMisuse: a programming error in the caller (unknown operation, the
	// wrong message type, a bad identity); nothing was sent.
	ErrMisuse = errors.New("intel: invalid call")
	// ErrNoHospital: the call needs a hospital and the caller has none.
	ErrNoHospital = errors.New("intel: no hospital for this call")
	// ErrBadFact: an audit fact a worker sent that core won't record.
	ErrBadFact = errors.New("intel: audit fact refused")
	// ErrAuditWrite: core couldn't append to the audit chain (the
	// database or a policy refused it): core's failure, not the worker's.
	ErrAuditWrite = errors.New("intel: audit append failed")
	// ErrCreatorUnusable: a job's creator can't use the service any more
	// (inactive, another hospital, hospital not running, no allowed role).
	ErrCreatorUnusable = errors.New("intel: the job's creator can no longer use the service")
)

// Error is a worker's answer when it refused or failed a call: a code from
// the closed set (authz.ErrorCodes) and a retry hint, nothing else.
type Error struct {
	Code      string
	Retryable bool
}

// Error names the code only when it is in the closed set, so a worker
// can't put text into a log through it.
func (e *Error) Error() string {
	if _, ok := workerCodes[e.Code]; ok {
		return "intel: worker answered " + e.Code
	}
	return "intel: worker answered an unknown code"
}

// mapping is a worker code's request/reply response: core's problem code
// and the Retry-After to send (0 for none).
type mapping struct {
	code       string
	retryAfter int
}

// workerCodes is the request/reply column of spec §10.1, the one table.
// unauthorized, forbidden and invalid_request all become 500
// internal_error: the user was already authorised by core, so a refusal at
// the boundary is a bug or an attack, and the answer must not say which
// check failed. cancelled is "—" in §10.1 (request/reply calls are never
// cancelled by a worker), so one that says so is unexpected: 500.
var workerCodes = map[string]mapping{
	"unauthorized":      {"internal_error", 0},
	"forbidden":         {"internal_error", 0},
	"invalid_request":   {"internal_error", 0},
	"not_found":         {"not_found", 0},
	"deadline_exceeded": {"unavailable", 0},
	"overloaded":        {"unavailable", 5},
	"unavailable":       {"unavailable", 0},
	"cancelled":         {"internal_error", 0},
	"internal":          {"internal_error", 0},
}

// refusalCodes are the worker codes audited as intelligence.refused (spec
// §10.2).
var refusalCodes = map[string]bool{"unauthorized": true, "forbidden": true, "invalid_request": true}

// The table must cover authz.ErrorCodes() exactly: a code added there
// without a mapping stops the binary at start-up, and the test fails first.
func init() {
	codes := authz.ErrorCodes()
	for _, c := range codes {
		if _, ok := workerCodes[c.Code]; !ok {
			panic(fmt.Sprintf("intel: worker code %q has no mapping", c.Code))
		}
	}
	if len(codes) != len(workerCodes) {
		panic("intel: the mapping has codes authz.ErrorCodes doesn't")
	}
}

// Code maps err from Request, PublishJob or ReserveInFlight to the problem
// code to send and the Retry-After seconds (0 for none). nil is "". An
// unknown worker code, and any error not listed, is internal_error.
func Code(err error) (code string, retryAfter int) {
	if err == nil {
		return "", 0
	}
	var we *Error
	switch {
	case errors.As(err, &we):
		if m, ok := workerCodes[we.Code]; ok {
			return m.code, m.retryAfter
		}
		return "internal_error", 0
	case errors.Is(err, ErrTimeout), errors.Is(err, ErrNoResponders),
		errors.Is(err, ErrBackpressure), errors.Is(err, ErrPublish):
		return "unavailable", 0
	case errors.Is(err, ErrAtCap):
		return "rate_limited", 0
	case errors.Is(err, ErrNotAllowed):
		return "forbidden", 0
	}
	return "internal_error", 0
}

// Refusal reports whether err is a worker refusal (unauthorized, forbidden
// or invalid_request), and its code.
func Refusal(err error) (string, bool) {
	var we *Error
	if errors.As(err, &we) && refusalCodes[we.Code] {
		return we.Code, true
	}
	return "", false
}
