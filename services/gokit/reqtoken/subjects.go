package reqtoken

import "strings"

// SubjectRule is one subject shape a request token may name (plan
// departure 4). Every allowed subject has four dot-separated segments:
// Prefix is the first ones, fixed; the last is the operation or job type,
// a name.
type SubjectRule struct {
	Kind   Kind
	Prefix string // the fixed segments, ending in "."
	// AudFromSegment is the segment that names the audience (the service),
	// or -1 when the subject fixes no segment: then Aud, if set, is the
	// only audience, and "" means any (jobs: the consumer is the stream's).
	AudFromSegment int
	Aud            string
	// HospitalSegment is the segment that names the hospital (a uuid, or
	// "global"), or -1. Mint and verify bind it to the hid claim.
	HospitalSegment int
	// CoreOnly: only core calls this subject (via core, reply on
	// _INBOX_gateway.); no grant (BFF-direct) token is valid on it.
	CoreOnly bool
	// StepUp: a token on this subject may carry the stp claim.
	StepUp bool
}

// subjectSegments is the length of every allowed subject.
const subjectSegments = 4

// SubjectRules are the subject shapes core may sign and a verifier accepts:
//   - inference.v1.<service>.<op>: request/reply to a worker; the service
//     is the audience;
//   - emr.v1.ctl.<op>: core's control calls to the emr service (EMR design
//     §3.2); the audience is always emr, and only core calls them;
//   - jobs.v1.<hid|global>.<type>: JetStream jobs, bound to the hospital.
var SubjectRules = []SubjectRule{
	{Kind: KindReq, Prefix: "inference.v1.", AudFromSegment: 2, HospitalSegment: -1},
	{Kind: KindReq, Prefix: "emr.v1.ctl.", AudFromSegment: -1, Aud: "emr", HospitalSegment: -1, CoreOnly: true, StepUp: true},
	{Kind: KindJob, Prefix: "jobs.v1.", AudFromSegment: -1, HospitalSegment: 2},
}

// ruleFor returns the rule subject matches for kind, and its segments.
func ruleFor(kind Kind, subject string) (SubjectRule, []string, bool) {
	seg := strings.Split(subject, ".")
	if len(seg) != subjectSegments || !isName(seg[subjectSegments-1]) {
		return SubjectRule{}, nil, false
	}
	for _, r := range SubjectRules {
		if r.Kind != kind || !strings.HasPrefix(subject, r.Prefix) {
			continue
		}
		fixed := strings.Count(r.Prefix, ".")
		for i := fixed; i < subjectSegments-1; i++ {
			switch {
			case i == r.AudFromSegment:
				if !isName(seg[i]) {
					return SubjectRule{}, nil, false
				}
			case i == r.HospitalSegment:
				if seg[i] != "global" && !isUUID(seg[i]) {
					return SubjectRule{}, nil, false
				}
			default:
				return SubjectRule{}, nil, false // a segment no rule names
			}
		}
		return r, seg, true
	}
	return SubjectRule{}, nil, false
}

// AudienceFor returns the audience subject fixes for a token of kind: the
// service segment of inference.v1.<service>.<op>, "emr" for
// emr.v1.ctl.<op>, and "" (any) for a job. ok is false for a subject no
// rule allows.
func AudienceFor(kind Kind, subject string) (aud string, ok bool) {
	r, seg, ok := ruleFor(kind, subject)
	if !ok {
		return "", false
	}
	if r.AudFromSegment >= 0 {
		return seg[r.AudFromSegment], true
	}
	return r.Aud, true
}

// checkSubject is the binding of a token's subject to its other claims,
// for mint and verify alike: the subject has an allowed shape for kind,
// the audience is the one it fixes, the hospital segment (if any) is
// "global" or hid, and a core-only subject is via core.
func checkSubject(kind Kind, subject, aud, hid string, via Via) bool {
	r, seg, ok := ruleFor(kind, subject)
	if !ok {
		return false
	}
	want := r.Aud
	if r.AudFromSegment >= 0 {
		want = seg[r.AudFromSegment]
	}
	if want != "" && aud != want {
		return false
	}
	if r.HospitalSegment >= 0 && seg[r.HospitalSegment] != "global" && seg[r.HospitalSegment] != hid {
		return false
	}
	if r.CoreOnly && via != ViaCore {
		return false
	}
	return true
}

// stepUpAllowed reports whether a token of kind on subject may carry the
// stp claim: only a subject whose rule says so.
func stepUpAllowed(kind Kind, subject string) bool {
	r, _, ok := ruleFor(kind, subject)
	return ok && r.StepUp
}
