package auth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// MFAToken is a pending two-step sign-in. It is kept in the MFA attempts
// bucket under "mfatok." + sha256(token); the token itself is never stored.
type MFAToken struct {
	UserID       string    `json:"uid"`
	ExpiresAt    time.Time `json:"exp"`
	AttemptsLeft int       `json:"left"`
	// Enrolled is whether the account had two-step set up at sign-in;
	// Task 7's challenge sends an unenrolled account to enrolment.
	Enrolled bool `json:"enrolled"`
	// DeviceAllowed is whether the hospital lets this account trust a device.
	DeviceAllowed bool `json:"device_allowed"`
}

// ErrMFAToken is every reason an mfa_token is unusable: unknown, expired,
// already used or out of attempts.
var ErrMFAToken = errors.New("auth: mfa token not valid")

// MFATokens stores pending two-step sign-ins.
type MFATokens struct {
	kv  jetstream.KeyValue
	now func() time.Time
}

// MFATokens returns the store behind the service's clock.
func (s *Service) MFATokens() *MFATokens {
	return &MFATokens{kv: s.Buckets.MFAAttempts, now: s.now}
}

func mfaKey(tok string) string { return "mfatok." + token.HashRefresh(tok) }

// Issue makes a new single-use token for rec (5 minutes, 5 attempts).
func (m *MFATokens) Issue(ctx context.Context, rec MFAToken) (string, error) {
	tok := token.NewOpaque()
	rec.ExpiresAt = m.now().Add(MFATokenTTL)
	rec.AttemptsLeft = MFATokenAttempts
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	if _, err := m.kv.Create(ctx, mfaKey(tok), raw); err != nil {
		return "", errors.New("auth: mfa token store failed")
	}
	return tok, nil
}

// get reads a live record. minLeft is the fewest attempts left it accepts:
// 1 to make an attempt or look one up, 0 to consume (a correct code on the
// last attempt).
func (m *MFATokens) get(ctx context.Context, tok string, minLeft int) (MFAToken, uint64, error) {
	if tok == "" || len(tok) > 128 {
		return MFAToken{}, 0, ErrMFAToken
	}
	e, err := m.kv.Get(ctx, mfaKey(tok))
	if bus.IsNotFound(err) {
		return MFAToken{}, 0, ErrMFAToken
	}
	if err != nil {
		return MFAToken{}, 0, errors.New("auth: mfa token read failed")
	}
	var rec MFAToken
	if json.Unmarshal(e.Value(), &rec) != nil || rec.AttemptsLeft < minLeft || rec.AttemptsLeft > MFATokenAttempts ||
		!m.now().Before(rec.ExpiresAt) {
		return MFAToken{}, 0, ErrMFAToken
	}
	return rec, e.Revision(), nil
}

// Lookup returns a usable token's record (at least one attempt left)
// without spending anything.
func (m *MFATokens) Lookup(ctx context.Context, tok string) (MFAToken, error) {
	rec, _, err := m.get(ctx, tok, 1)
	return rec, err
}

// Attempt spends one attempt by compare-and-set before the caller checks a
// code, so a concurrent burst gets at most MFATokenAttempts checks. It
// returns the record with AttemptsLeft after this attempt. Any error,
// including a KV failure or lost races past the retries, means no check may
// run (fail closed). On a correct code the caller then calls Consume.
func (m *MFATokens) Attempt(ctx context.Context, tok string) (MFAToken, error) {
	for i := 0; i < 16; i++ {
		rec, rev, err := m.get(ctx, tok, 1)
		if err != nil {
			return MFAToken{}, err
		}
		rec.AttemptsLeft--
		raw, err := json.Marshal(rec)
		if err != nil {
			return MFAToken{}, err
		}
		_, err = m.kv.Update(ctx, mfaKey(tok), raw, rev)
		if err == nil {
			return rec, nil
		}
		if !isRevisionConflict(err) {
			return MFAToken{}, errors.New("auth: mfa token write failed")
		}
	}
	return MFAToken{}, ErrMFAToken
}

// Consume spends the token after a correct code: the first caller gets the
// record, any other (or a concurrent one) gets ErrMFAToken.
func (m *MFATokens) Consume(ctx context.Context, tok string) (MFAToken, error) {
	rec, rev, err := m.get(ctx, tok, 0)
	if err != nil {
		return MFAToken{}, err
	}
	if err := m.kv.Delete(ctx, mfaKey(tok), jetstream.LastRevision(rev)); err != nil {
		return MFAToken{}, ErrMFAToken
	}
	return rec, nil
}
