// Package bus connects the gateway to NATS and opens the JetStream KV
// buckets that hold short-lived shared state.
//
// Keys are opaque strings. Callers pass a sha256 hex digest, never an email
// or an address (ruling 3), and nothing in this package logs a key or a
// value.
package bus

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Inbox prefixes. Each service may subscribe only to its own, so a worker
// never sees replies meant for the gateway (and cannot forge them).
const (
	GatewayInboxPrefix = "_INBOX_gateway"
	WorkerInboxPrefix  = "_INBOX_worker"
)

// Bucket names and lifetimes.
const (
	DenylistBucket      = "denylist"
	LoginAttemptsBucket = "login_attempts"
	MFAAttemptsBucket   = "mfa_attempts"

	DenylistTTL      = 15 * time.Minute
	LoginAttemptsTTL = 15 * time.Minute
	MFAAttemptsTTL   = 15 * time.Minute
)

// Buckets are the gateway's KV stores.
type Buckets struct {
	Denylist      jetstream.KeyValue // revoked token ids, until the token would have expired
	LoginAttempts jetstream.KeyValue // sign-in failure counters
	MFAAttempts   jetstream.KeyValue // step-up code failure counters
}

// Errors from Connect are fixed messages or known sentinels; they never
// contain the URL, a password or a file path.
var (
	errInvalidURL  = errors.New("bus: invalid NATS url")
	errSeed        = errors.New("bus: cannot use nkey seed file")
	errConnectFail = errors.New("bus: connect failed")
)

// Connect dials NATS and returns the connection and its JetStream handle.
//
// url may carry user:password (the dev server); they are taken out of the
// URL and passed as options. nkeySeedPath, when set, is the service's NKey
// seed file (production). inboxPrefix is the service's own reply prefix, for
// example GatewayInboxPrefix. ctx is checked on entry; the dial itself is
// bounded by a 5 s timeout.
func Connect(ctx context.Context, rawURL, nkeySeedPath, inboxPrefix string) (*nats.Conn, jetstream.JetStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var servers []string
	var user, pass string
	var haveUser bool
	for _, part := range strings.Split(rawURL, ",") {
		u, err := url.Parse(strings.TrimSpace(part))
		if err != nil || u.Host == "" {
			return nil, nil, errInvalidURL
		}
		if u.User != nil {
			user = u.User.Username()
			pass, _ = u.User.Password()
			haveUser = true
			u.User = nil
		}
		servers = append(servers, u.String())
	}
	opts := []nats.Option{
		nats.Name("mhai-" + strings.TrimPrefix(inboxPrefix, "_INBOX_")),
		nats.Timeout(5 * time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	}
	if inboxPrefix != "" {
		opts = append(opts, nats.CustomInboxPrefix(inboxPrefix))
	}
	if haveUser {
		opts = append(opts, nats.UserInfo(user, pass))
	}
	if nkeySeedPath != "" {
		o, err := nats.NkeyOptionFromSeed(nkeySeedPath)
		if err != nil {
			return nil, nil, errSeed
		}
		opts = append(opts, o)
	}
	nc, err := nats.Connect(strings.Join(servers, ","), opts...)
	if err != nil {
		switch {
		case errors.Is(err, nats.ErrAuthorization):
			return nil, nil, nats.ErrAuthorization
		case errors.Is(err, nats.ErrNoServers):
			return nil, nil, nats.ErrNoServers
		case errors.Is(err, nats.ErrTimeout):
			return nil, nil, nats.ErrTimeout
		}
		return nil, nil, errConnectFail
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, errConnectFail
	}
	return nc, js, nil
}

// OpenBucket creates the KV bucket, or updates it to this lifetime, and
// returns it. Entries expire ttl after they were last written (NATS 2.10 has
// no per-key TTL); one revision is kept.
func OpenBucket(ctx context.Context, js jetstream.JetStream, name string, ttl time.Duration) (jetstream.KeyValue, error) {
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  name,
		TTL:     ttl,
		History: 1,
		Storage: jetstream.FileStorage,
	})
	if err != nil {
		return nil, errors.New("bus: open bucket " + name + " failed")
	}
	return kv, nil
}

// OpenBuckets opens the three buckets, creating any that are missing.
func OpenBuckets(ctx context.Context, js jetstream.JetStream) (Buckets, error) {
	var b Buckets
	var err error
	if b.Denylist, err = OpenBucket(ctx, js, DenylistBucket, DenylistTTL); err != nil {
		return Buckets{}, err
	}
	if b.LoginAttempts, err = OpenBucket(ctx, js, LoginAttemptsBucket, LoginAttemptsTTL); err != nil {
		return Buckets{}, err
	}
	if b.MFAAttempts, err = OpenBucket(ctx, js, MFAAttemptsBucket, MFAAttemptsTTL); err != nil {
		return Buckets{}, err
	}
	return b, nil
}

// ErrNotFound reports a missing key, for callers that don't import jetstream.
var ErrNotFound = jetstream.ErrKeyNotFound

// IsNotFound reports whether err is a missing key.
func IsNotFound(err error) bool { return errors.Is(err, jetstream.ErrKeyNotFound) }
