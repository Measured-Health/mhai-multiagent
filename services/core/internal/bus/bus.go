// Package bus connects the core service to NATS, opens the JetStream KV
// buckets that hold short-lived shared state, and makes the JOBS and DEAD
// streams with their durable consumers.
//
// Keys are opaque strings. Callers pass a sha256 hex digest, never an email
// or an address (ruling 3), and nothing in this package logs a key or a
// value.
package bus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Inbox prefixes. Each NATS user may subscribe only to its own, so a worker
// never sees replies meant for core or another worker, and the BFFs never
// see each other's (services/nats/nats.conf).
const (
	GatewayInboxPrefix = "_INBOX_gateway"
	WorkerInboxPrefix  = "_INBOX_worker" // + "." + service: see WorkerInbox
	BFFInboxPrefix     = "_INBOX_bff"    // + "." + app: see BFFInbox
)

// WorkerInbox is the inbox prefix of the intelligence service named
// service (for example "_INBOX_worker.pipeline").
func WorkerInbox(service string) string { return WorkerInboxPrefix + "." + service }

// BFFInbox is the inbox prefix of the BFF for app ("clinical" or
// "platform").
func BFFInbox(app string) string { return BFFInboxPrefix + "." + app }

// ReplyUnder reports whether reply, a requester-chosen reply subject, is
// prefix (which ends in ".") followed by one or more literal tokens:
// non-empty, printable ASCII, no '*', '>' or space; and at most max bytes.
//
// Every responder checks this before answering (services/nats/README.md,
// "Responders must check the reply subject"): NATS lets the requester set
// the reply subject, and allow_responses lets the responder publish there
// once with its own permissions. Core may publish to JOBS, $KV and $JS,
// so an unchecked reply would let a requester steer core's one publish
// anywhere.
func ReplyUnder(prefix, reply string, max int) bool {
	if !strings.HasSuffix(prefix, ".") || len(reply) > max || !strings.HasPrefix(reply, prefix) {
		return false
	}
	for _, tok := range strings.Split(reply[len(prefix):], ".") {
		if tok == "" {
			return false
		}
		for i := 0; i < len(tok); i++ {
			if c := tok[i]; c <= ' ' || c >= 0x7f || c == '*' || c == '>' {
				return false
			}
		}
	}
	return true
}

// Bucket names and lifetimes.
const (
	DenylistBucket      = "denylist"
	LoginAttemptsBucket = "login_attempts"
	MFAAttemptsBucket   = "mfa_attempts"
	LoginCeilingBucket  = "login_ceiling"

	DenylistTTL      = 17 * time.Minute // access token life (15 min) plus clock-skew margin
	LoginAttemptsTTL = 15 * time.Minute
	MFAAttemptsTTL   = 15 * time.Minute
	LoginCeilingTTL  = time.Hour // per-login failures across every client IP
)

// Buckets are the gateway's KV stores.
type Buckets struct {
	Denylist      jetstream.KeyValue // revoked token ids, until the token would have expired
	LoginAttempts jetstream.KeyValue // sign-in failure counters
	MFAAttempts   jetstream.KeyValue // step-up code failure counters
	LoginCeiling  jetstream.KeyValue // sign-in failure counters per login, across IPs
}

// All returns every bucket with its name, for readiness checks.
func (b Buckets) All() map[string]jetstream.KeyValue {
	return map[string]jetstream.KeyValue{
		DenylistBucket: b.Denylist, LoginAttemptsBucket: b.LoginAttempts,
		MFAAttemptsBucket: b.MFAAttempts, LoginCeilingBucket: b.LoginCeiling,
	}
}

// Errors from Connect are fixed messages or known sentinels; they never
// contain the URL, a password or a file path.
var (
	errInvalidURL  = errors.New("bus: invalid NATS url")
	errSeed        = errors.New("bus: cannot use nkey seed file")
	errTLS         = errors.New("bus: cannot use NATS TLS files")
	errConnectFail = errors.New("bus: connect failed")
)

// TLS names the PEM files for a TLS connection to NATS: the CA that signs
// the server's certificate, and this service's client certificate and key
// (the server verifies them in production). All three or none.
type TLS struct {
	CAPath   string // NATS_TLS_CA_PATH
	CertPath string // NATS_TLS_CERT_PATH
	KeyPath  string // NATS_TLS_KEY_PATH
}

// Enabled reports whether any TLS file is set.
func (t TLS) Enabled() bool { return t.CAPath != "" || t.CertPath != "" || t.KeyPath != "" }

// config builds the client TLS config: only the named CA is trusted (not
// the system roots), TLS 1.2 at least, with the client certificate.
// Errors are fixed messages; they never name a path.
func (t TLS) config() (*tls.Config, error) {
	if t.CAPath == "" || t.CertPath == "" || t.KeyPath == "" {
		return nil, errTLS
	}
	ca, err := os.ReadFile(t.CAPath)
	if err != nil {
		return nil, errTLS
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errTLS
	}
	cert, err := tls.LoadX509KeyPair(t.CertPath, t.KeyPath)
	if err != nil {
		return nil, errTLS
	}
	return &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// Connect dials NATS and returns the connection and its JetStream handle.
//
// url may carry user:password (the dev server); they are taken out of the
// URL and passed as options. nkeySeedPath, when set, is the service's NKey
// seed file (production). inboxPrefix is the service's own reply prefix, for
// example GatewayInboxPrefix. tlsFiles, when set, makes the connection TLS
// with a client certificate (production requires it; config.ValidateServe).
// ctx is checked on entry; the dial itself is bounded by a 5 s timeout.
func Connect(ctx context.Context, rawURL, nkeySeedPath, inboxPrefix string, tlsFiles TLS) (*nats.Conn, jetstream.JetStream, error) {
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
	if tlsFiles.Enabled() {
		tc, err := tlsFiles.config()
		if err != nil {
			return nil, nil, err
		}
		opts = append(opts, nats.Secure(tc))
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

// OpenBucket creates the KV bucket, or updates it to this lifetime and
// replica count, and returns it. Entries expire ttl after they were last
// written (NATS 2.10 has no per-key TTL); one revision is kept. The NATS
// user may open only the four buckets below (services/nats/nats.conf).
func OpenBucket(ctx context.Context, js jetstream.JetStream, name string, ttl time.Duration, replicas int) (jetstream.KeyValue, error) {
	replicas, err := replicaCount(replicas)
	if err != nil {
		return nil, err
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   name,
		TTL:      ttl,
		History:  1,
		Storage:  jetstream.FileStorage,
		Replicas: replicas,
	})
	if err != nil {
		return nil, errors.New("bus: open bucket " + name + " failed")
	}
	return kv, nil
}

// OpenBuckets opens the four buckets, creating any that are missing, with
// replicas copies each (NATS_JS_REPLICAS: 1 in dev, 3 in production).
func OpenBuckets(ctx context.Context, js jetstream.JetStream, replicas int) (Buckets, error) {
	var b Buckets
	var err error
	if b.Denylist, err = OpenBucket(ctx, js, DenylistBucket, DenylistTTL, replicas); err != nil {
		return Buckets{}, err
	}
	if b.LoginAttempts, err = OpenBucket(ctx, js, LoginAttemptsBucket, LoginAttemptsTTL, replicas); err != nil {
		return Buckets{}, err
	}
	if b.MFAAttempts, err = OpenBucket(ctx, js, MFAAttemptsBucket, MFAAttemptsTTL, replicas); err != nil {
		return Buckets{}, err
	}
	if b.LoginCeiling, err = OpenBucket(ctx, js, LoginCeilingBucket, LoginCeilingTTL, replicas); err != nil {
		return Buckets{}, err
	}
	return b, nil
}

// ErrNotFound reports a missing key, for callers that don't import jetstream.
var ErrNotFound = jetstream.ErrKeyNotFound

// IsNotFound reports whether err is a missing key.
func IsNotFound(err error) bool { return errors.Is(err, jetstream.ErrKeyNotFound) }
