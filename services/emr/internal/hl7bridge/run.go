package hl7bridge

// Configuration, the host key, health and the run loop.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config is the bridge's configuration (HL7_BRIDGE_* variables; deploy/
// hl7-bridge/README.md).
type Config struct {
	MLLPAddr, SFTPAddr, HealthAddr string
	MLLPAllow, SFTPAllow           *Allowlist
	TLSCert, TLSKey, TLSClientCA   string
	SFTPUser                       string
	AuthorizedKey                  ssh.PublicKey
	Spool, State                   string
	Rotate, Retain                 time.Duration
}

// MinRetain is the shortest retention of processed files.
const MinRetain = 24 * time.Hour

// ErrConfig is a configuration refusal; the text names the variable, never
// a value.
type ErrConfig struct{ Var string }

func (e *ErrConfig) Error() string { return "hl7-bridge: " + e.Var + " is missing or not valid" }

// ConfigFromEnv reads the configuration from getenv.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	def := func(k, d string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return d
	}
	c := Config{MLLPAddr: def("HL7_BRIDGE_MLLP_ADDR", ":2575"), SFTPAddr: def("HL7_BRIDGE_SFTP_ADDR", ":2222"),
		HealthAddr: def("HL7_BRIDGE_HEALTH_ADDR", "127.0.0.1:8080"), TLSCert: getenv("HL7_BRIDGE_MLLP_TLS_CERT"),
		TLSKey: getenv("HL7_BRIDGE_MLLP_TLS_KEY"), TLSClientCA: getenv("HL7_BRIDGE_MLLP_TLS_CLIENT_CA"),
		SFTPUser: getenv("HL7_BRIDGE_SFTP_USER"), Spool: def("HL7_BRIDGE_SPOOL", "/spool"),
		State: def("HL7_BRIDGE_STATE", "/state")}
	var err error
	if c.MLLPAllow, err = ParseAllowlist(getenv("HL7_BRIDGE_MLLP_ALLOW")); err != nil {
		return c, &ErrConfig{"HL7_BRIDGE_MLLP_ALLOW"}
	}
	if c.SFTPAllow, err = ParseAllowlist(getenv("HL7_BRIDGE_SFTP_ALLOW")); err != nil {
		return c, &ErrConfig{"HL7_BRIDGE_SFTP_ALLOW"}
	}
	if !userShape(c.SFTPUser) {
		return c, &ErrConfig{"HL7_BRIDGE_SFTP_USER"}
	}
	kb, err := os.ReadFile(getenv("HL7_BRIDGE_AUTHORIZED_KEY"))
	if err != nil {
		return c, &ErrConfig{"HL7_BRIDGE_AUTHORIZED_KEY"}
	}
	k, _, _, rest, err := ssh.ParseAuthorizedKey(kb)
	if err != nil || len(trimSpace(rest)) != 0 || !strongKey(k) {
		return c, &ErrConfig{"HL7_BRIDGE_AUTHORIZED_KEY"}
	}
	c.AuthorizedKey = k
	if c.Rotate, err = time.ParseDuration(def("HL7_BRIDGE_ROTATE", "1m")); err != nil || c.Rotate < time.Second || c.Rotate > time.Hour {
		return c, &ErrConfig{"HL7_BRIDGE_ROTATE"}
	}
	if c.Retain, err = time.ParseDuration(def("HL7_BRIDGE_RETAIN", "168h")); err != nil || c.Retain < MinRetain {
		return c, &ErrConfig{"HL7_BRIDGE_RETAIN"}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") || (c.TLSClientCA != "" && c.TLSCert == "") {
		return c, &ErrConfig{"HL7_BRIDGE_MLLP_TLS_CERT"}
	}
	return c, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\n' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

func userShape(u string) bool {
	if u == "" || len(u) > 32 {
		return false
	}
	for i := 0; i < len(u); i++ {
		c := u[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return u[0] != '-' && u[0] != '.'
}

// strongKey: Ed25519, ECDSA, or RSA of at least 2048 bits (the connector's
// own rule for the private half).
func strongKey(k ssh.PublicKey) bool {
	switch k.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return true
	case ssh.KeyAlgoRSA:
		if cp, ok := k.(ssh.CryptoPublicKey); ok {
			if rk, ok := cp.CryptoPublicKey().(interface{ Size() int }); ok {
				return rk.Size()*8 >= 2048
			}
		}
	}
	return false
}

const hostKeyName = "ssh_host_ed25519_key"

// HostKey loads the bridge's SSH host key from state, making it on first
// start (0600). Its fingerprint is what the hospital gives the platform
// admin.
func HostKey(state string) (ssh.Signer, error) {
	p := filepath.Join(state, hostKeyName)
	if b, err := os.ReadFile(p); err == nil {
		return ssh.ParsePrivateKey(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, err
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(k, "")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(pem.EncodeToMemory(blk)); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(k)
}

// Health is the /healthz body: counts and times only.
type Health struct {
	Status        string `json:"status"` // ok, degraded, failing
	PendingFiles  int    `json:"pending_files"`
	RejectedFiles int    `json:"rejected_files"`
	LastMessageAt string `json:"last_message_at,omitempty"`
	LastPullAt    string `json:"last_pull_at,omitempty"`
	DiskOK        bool   `json:"disk_ok"`
}

// HealthOf reads the spool's state.
func HealthOf(sp *Spool, counts *Counters, sftp *SFTPServer, now time.Time) Health {
	h := Health{Status: "ok", DiskOK: !sp.Broken()}
	h.PendingFiles, h.RejectedFiles = sp.Counts()
	at := func(sec int64) string {
		if sec == 0 {
			return ""
		}
		return time.Unix(sec, 0).UTC().Format(time.RFC3339)
	}
	var lastPull int64
	if sftp != nil {
		lastPull = sftp.LastPull.Load()
	}
	if counts != nil {
		h.LastMessageAt = at(counts.LastMessage.Load())
	}
	h.LastPullAt = at(lastPull)
	switch {
	case !h.DiskOK || h.PendingFiles < 0:
		h.Status = "failing"
	case h.RejectedFiles != 0, h.PendingFiles > 0 && now.Unix()-lastPull > 3600:
		h.Status = "degraded"
	}
	return h
}

// HealthHandler serves GET /healthz.
func HealthHandler(sp *Spool, counts *Counters, sftp *SFTPServer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		h := HealthOf(sp, counts, sftp, time.Now())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if h.Status == "failing" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(h)
	})
	return mux
}

func (c Config) mllpListener() (net.Listener, error) {
	ln, err := net.Listen("tcp", c.MLLPAddr)
	if err != nil || c.TLSCert == "" {
		return ln, err
	}
	cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
	if err != nil {
		_ = ln.Close()
		return nil, &ErrConfig{"HL7_BRIDGE_MLLP_TLS_CERT"}
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	if c.TLSClientCA != "" {
		b, err := os.ReadFile(c.TLSClientCA)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(b) {
			_ = ln.Close()
			return nil, &ErrConfig{"HL7_BRIDGE_MLLP_TLS_CLIENT_CA"}
		}
		tc.ClientCAs, tc.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return tls.NewListener(ln, tc), nil
}

// Run serves until ctx ends, then closes the open spool file.
func Run(ctx context.Context, c Config, log *slog.Logger) error {
	sp := &Spool{Root: c.Spool, State: c.State}
	if err := sp.Open(); err != nil {
		return err
	}
	defer func() { _ = sp.Close() }()
	hk, err := HostKey(c.State)
	if err != nil {
		return errors.New("hl7-bridge: the host key can't be read or made")
	}
	counts := &Counters{}
	srv := &SFTPServer{Spool: sp, HostKey: hk, Authorized: c.AuthorizedKey, User: c.SFTPUser, Allow: c.SFTPAllow, Log: log}
	rcv := &Receiver{Spool: sp, Allow: c.MLLPAllow, Log: log, Counts: counts}
	mln, err := c.mllpListener()
	if err != nil {
		return err
	}
	sln, err := net.Listen("tcp", c.SFTPAddr)
	if err != nil {
		_ = mln.Close()
		return err
	}
	hs := &http.Server{Addr: c.HealthAddr, Handler: HealthHandler(sp, counts, srv), ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 3)
	go func() { errs <- rcv.Serve(ctx, mln) }()
	go func() { errs <- srv.Serve(ctx, sln) }()
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	go func() { <-ctx.Done(); _ = hs.Close() }()
	log.Info("started", "host_key_fingerprint", ssh.FingerprintSHA256(hk.PublicKey()))
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return err
		case <-tick.C:
			if err := sp.Rotate(c.Rotate); err != nil {
				log.Error("spool_rotate_failed")
			}
			if time.Since(lastPrune) > time.Hour {
				lastPrune = time.Now()
				if n := sp.Prune(c.Retain); n > 0 {
					log.Info("pruned", "files", n)
				}
			}
		}
	}
}
