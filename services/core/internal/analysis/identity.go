package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
)

// Identity: names come from the EMR service at the moment of viewing
// (design §8.6; plan Task 23). Core never stores, caches, logs or traces a
// name or an MRN: a Resolver's reply is passed into one response and
// dropped.

// Identity is one patient's name and MRN. It never prints: String,
// GoString, LogValue and Format are redacted; Name and MRN are the
// reply writer's only way to it.
type Identity struct {
	name, mrn string
}

// NewIdentity is an identity a resolver read.
func NewIdentity(name, mrn string) Identity { return Identity{name: name, mrn: mrn} }

// Name and MRN are for the reply writer only.
func (i Identity) Name() string { return i.name }
func (i Identity) MRN() string  { return i.mrn }

func (Identity) String() string       { return "analysis.Identity{redacted}" }
func (Identity) GoString() string     { return "analysis.Identity{redacted}" }
func (Identity) LogValue() slog.Value { return slog.StringValue("redacted") }

// Format covers every verb.
func (i Identity) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(i.String())) }

// IdentityRequest is one named list's identity call: the hospital and the
// person from the verified token, the list and the letter, and the
// patient-index references (decrypted for this call only), at most 200.
type IdentityRequest struct {
	Hospital, User, View, Clearance uuid.UUID
	// Demo: the letter is a demo letter (clearances.demo).
	Demo bool
	Refs []string
}

// Resolver resolves exactly the asked references.
type Resolver interface {
	Resolve(ctx context.Context, r IdentityRequest) (map[string]Identity, error)
}

var (
	// ErrIdentityUnavailable: no identity source (503 unavailable).
	ErrIdentityUnavailable = errors.New("analysis: identity unavailable")
	// ErrIdentityReply: the reply wasn't exactly the asked references (T20).
	ErrIdentityReply = errors.New("analysis: identity reply refused")
	// errDemoLive: the demo fixture with live identity configured (T27).
	errDemoLive = errors.New("analysis: the demo identity fixture refuses to start with the EMR integration configured")
)

// MaxIdentities is the most references one call asks (DrillMax).
const MaxIdentities = 200

// Unavailable is the live resolver until the EMR service has its identity
// operation (H7; plan departure 46): every call is refused.
type Unavailable struct{}

func (Unavailable) Resolve(context.Context, IdentityRequest) (map[string]Identity, error) {
	return nil, ErrIdentityUnavailable
}

// CheckIdentities accepts got only when it holds exactly the asked
// references, at most MaxIdentities, each with a name (T20): an extra
// entry, a missing one or one on another reference fails the whole list.
func CheckIdentities(asked []string, got map[string]Identity) error {
	if len(asked) == 0 || len(asked) > MaxIdentities || len(got) != len(asked) {
		return ErrIdentityReply
	}
	seen := make(map[string]bool, len(asked))
	for _, r := range asked {
		if seen[r] {
			return ErrIdentityReply
		}
		seen[r] = true
		id, ok := got[r]
		if !ok || id.name == "" {
			return ErrIdentityReply
		}
	}
	return nil
}

// DemoIdentity is the synthetic fixture the demo resolves names from
// (design §8.6): names and MRNs drawn from the reference, marked as
// synthetic, for a demo letter's list only.
type DemoIdentity struct{}

// NewDemoIdentity refuses with live identity configured (T27): emrEnabled
// is the deployment's EMR integration (CORE_EMR_ENABLED).
func NewDemoIdentity(emrEnabled bool) (*DemoIdentity, error) {
	if emrEnabled {
		return nil, errDemoLive
	}
	return &DemoIdentity{}, nil
}

var (
	demoGiven  = []string{"Zephyrine", "Quillon", "Marisol", "Thadeus", "Ondine", "Peregrin", "Calliope", "Bartholo"}
	demoFamily = []string{"Thistlewood", "Ombrevale", "Fernquist", "Halloway", "Brindlecombe", "Starrow", "Quenby", "Vantreece"}
)

// Resolve answers each reference with a synthetic name and MRN; a list
// whose letter isn't a demo letter is refused.
func (*DemoIdentity) Resolve(_ context.Context, r IdentityRequest) (map[string]Identity, error) {
	if !r.Demo {
		return nil, ErrIdentityUnavailable
	}
	if len(r.Refs) == 0 || len(r.Refs) > MaxIdentities {
		return nil, ErrIdentityReply
	}
	out := make(map[string]Identity, len(r.Refs))
	for _, ref := range r.Refs {
		h := sha256.Sum256([]byte("analysis/demo-identity/v1\x00" + ref))
		n := binary.BigEndian.Uint32(h[:4])
		out[ref] = Identity{
			name: demoGiven[int(h[4])%len(demoGiven)] + " " + demoFamily[int(h[5])%len(demoFamily)] + " (synthetic)",
			mrn:  fmt.Sprintf("SYN-%08d", n%100000000),
		}
	}
	return out, nil
}
