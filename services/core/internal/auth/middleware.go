package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/bus"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/token"
)

// caller is who an authenticated auth route acts for, as strings: the
// principal the authz middleware built from the verified token. Sub is the
// user id, Hid the hospital id ("" for platform accounts), Sid the session
// id and Jti the access token id.
type caller struct {
	Sub, Hid, Sid, Jti string
}

// callerOf returns the request's principal as a caller. Every route that
// calls it is registered behind the principal middleware, so a missing
// principal yields empty ids, which the handlers refuse.
func callerOf(r *http.Request) caller {
	p, ok := authz.FromContext(r.Context())
	if !ok {
		return caller{}
	}
	c := caller{Sub: p.UserID.String(), Sid: p.SessionID, Jti: p.TokenID}
	if p.HospitalID != uuid.Nil {
		c.Hid = p.HospitalID.String()
	}
	return c
}

// denied reports whether c's jti or session is on the denylist.
func (s *Service) denied(ctx context.Context, c token.Claims) (bool, error) {
	for _, k := range []string{authz.JTIKey(c.Jti), authz.SIDKey(c.Sid)} {
		_, err := s.Buckets.Denylist.Get(ctx, k)
		if err == nil {
			return true, nil
		}
		if !bus.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

// Denylisted reports whether c is refused by the denylist (a read error
// counts as refused).
func (s *Service) Denylisted(ctx context.Context, c token.Claims) bool {
	d, err := s.denied(ctx, c)
	return d || err != nil
}

// denySession puts the session and the given access-token ids on the
// denylist. The bucket's 15-minute lifetime is at least the remaining life
// of any access token the session issued before it was revoked.
func (s *Service) denySession(ctx context.Context, sid string, jtis ...string) error {
	keys := []string{authz.SIDKey(sid)}
	for _, j := range jtis {
		if j != "" {
			keys = append(keys, authz.JTIKey(j))
		}
	}
	var errs []error
	for _, k := range keys {
		if _, err := s.Buckets.Denylist.Put(ctx, k, []byte("1")); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.New("auth: denylist write failed")
	}
	return nil
}
