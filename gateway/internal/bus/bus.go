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
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
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

// Connect dials NATS and returns the connection and its JetStream handle.
// url may carry user:password for the dev server; credsPath, when set, is a
// .creds file (production). Neither is logged or put in an error.
func Connect(ctx context.Context, url, credsPath string) (*nats.Conn, jetstream.JetStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	opts := []nats.Option{
		nats.Name("mhai-gateway"),
		nats.Timeout(5 * time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	}
	if credsPath != "" {
		opts = append(opts, nats.UserCredentials(credsPath))
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		// nats errors are sentinels (no-servers, auth); wrap without the URL.
		return nil, nil, fmt.Errorf("bus: connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("bus: jetstream: %w", err)
	}
	return nc, js, nil
}

// OpenBucket creates the KV bucket, or updates it to this lifetime, and
// returns it. Entries expire ttl after they were last written; one revision
// is kept.
func OpenBucket(ctx context.Context, js jetstream.JetStream, name string, ttl time.Duration) (jetstream.KeyValue, error) {
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  name,
		TTL:     ttl,
		History: 1,
		Storage: jetstream.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("bus: open bucket %s: %w", name, err)
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
