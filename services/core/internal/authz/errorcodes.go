package authz

// ErrorCode is one code of the closed set a worker answers with (spec §10.1).
// The Error message carries one of these and nothing else, so no free text
// can leak. Core's mapping of each code to a problem response lives with the
// callers; this table is the list the API docs publish.
type ErrorCode struct {
	Code    string
	Meaning string
}

var errorCodes = []ErrorCode{
	{"unauthorized", "A verification check failed (size, token, claims, binding, reply subject or replay)."},
	{"forbidden", "The role check failed: the operation's entry in the operations table allows none of the caller's roles."},
	{"invalid_request", "The schema or scope check failed."},
	{"not_found", "The row is not visible under the caller's hospital."},
	{"deadline_exceeded", "The call was received after its token expired."},
	{"overloaded", "The worker is at its concurrency limit."},
	{"unavailable", "A model or the database is not ready."},
	{"cancelled", "The job was cancelled."},
	{"internal", "Anything else."},
}

// ErrorCodes returns the closed set of worker error codes, in the order the
// docs show them. The caller gets a copy.
func ErrorCodes() []ErrorCode {
	return append([]ErrorCode(nil), errorCodes...)
}
