// Package natstls builds the client TLS configuration a Go service uses to
// reach NATS outside development: only the deployment's private CA is
// trusted (never the system roots), TLS 1.2 at least, and the service's own
// client certificate, which NATS verifies (tls { verify: true }). The NKey
// still says which NATS user the service is; the certificate says it is one
// of ours. Core has its own copy (services/core/internal/bus.TLS); the emr
// and notifications services use this one.
package natstls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
)

// Files names the three PEM files. All three or none.
type Files struct {
	CA, Cert, Key string
}

// Set reports whether any file is named.
func (f Files) Set() bool { return f.CA != "" || f.Cert != "" || f.Key != "" }

// Complete reports whether all three files are named.
func (f Files) Complete() bool { return f.CA != "" && f.Cert != "" && f.Key != "" }

// ErrFiles is the one error Config returns: it never names a path or
// carries a file's contents.
var ErrFiles = errors.New("natstls: the NATS TLS files are missing or not valid")

// Config reads the files and returns the client TLS configuration.
func (f Files) Config() (*tls.Config, error) {
	if !f.Complete() {
		return nil, ErrFiles
	}
	ca, err := os.ReadFile(f.CA)
	if err != nil {
		return nil, ErrFiles
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, ErrFiles
	}
	cert, err := tls.LoadX509KeyPair(f.Cert, f.Key)
	if err != nil {
		return nil, ErrFiles
	}
	return &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}
