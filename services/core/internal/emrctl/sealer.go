package emrctl

import (
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrpb"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// ErrSeal is the sealer's one error: no key, or a value or binding out of
// bounds. It names no value.
var ErrSeal = errors.New("emrctl: cannot seal")

// Sealer seals credentials to the emr service's public key
// (CORE_EMR_SEAL_PUBLIC_KEY_PATH). Core holds no private key.
type Sealer struct {
	pub *[32]byte
	kid string
	// Now dates the boxes; nil is time.Now.
	Now func() time.Time
}

// LoadSealer reads the public key file. Errors never name the path.
func LoadSealer(path string) (*Sealer, error) {
	pub, kid, err := seal.LoadPublic(path)
	if err != nil {
		return nil, errors.New("CORE_EMR_SEAL_PUBLIC_KEY_PATH: not a sealing public key (seal-<kid>.pub)")
	}
	return &Sealer{pub: pub, kid: kid}, nil
}

// NewSealer is a Sealer for pub (tests).
func NewSealer(pub [32]byte) *Sealer {
	p := pub
	return &Sealer{pub: &p, kid: seal.KeyID(p[:])}
}

// KeyID is the sealing key's id.
func (s *Sealer) KeyID() string { return s.kid }

// Seal seals value for the connection's field and origin. The caller zeroes
// value after.
func (s *Sealer) Seal(conn uuid.UUID, field string, origin []emrurl.Origin, value []byte) (*emrpb.SealedField, error) {
	if s == nil || s.pub == nil {
		return nil, ErrSeal
	}
	box, err := seal.Seal(s.pub, s.kid, seal.Binding{ConnectionID: conn, Field: field, Origin: origin}, value, s.now())
	if err != nil {
		return nil, ErrSeal
	}
	return &emrpb.SealedField{Field: field, Kid: s.kid, Box: box}, nil
}

func (s *Sealer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
