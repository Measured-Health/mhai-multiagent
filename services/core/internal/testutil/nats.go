package testutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nkeys"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

// NATSImage is the server every integration test runs against.
const NATSImage = "nats:2.10-alpine"

// Throwaway credentials for the test server only. Each user has its own,
// as in compose. The JetStream key starts with a letter (the config parser
// would read a leading digit as a number).
const (
	natsGatewayPassword     = "test-only-gateway-pw"
	natsPipelinePassword    = "test-only-pipeline-pw"
	natsEchoPassword        = "test-only-echo-pw"
	natsBFFClinicalPassword = "test-only-bff-clinical-pw"
	natsBFFPlatformPassword = "test-only-bff-platform-pw"
	natsEMRPassword         = "test-only-emr-pw"
	natsJSKey               = "testOnlyJetStreamKeyNotForAnyRealServer0123"
)

// NATSEnv is the environment the real nats.conf needs with the emr user on
// (emr.conf): every user's password and the JetStream encryption key.
// Exported so other packages' tests (and a Python fixture's documentation)
// use the same names. With the emr user off (emr-off.conf, the dev default)
// NATS_EMR_PASSWORD isn't needed.
func NATSEnv() map[string]string {
	return map[string]string{
		"NATS_GATEWAY_PASSWORD":      natsGatewayPassword,
		"NATS_PIPELINE_PASSWORD":     natsPipelinePassword,
		"NATS_ECHO_PASSWORD":         natsEchoPassword,
		"NATS_BFF_CLINICAL_PASSWORD": natsBFFClinicalPassword,
		"NATS_BFF_PLATFORM_PASSWORD": natsBFFPlatformPassword,
		"NATS_EMR_PASSWORD":          natsEMRPassword,
		"NATS_JS_KEY":                natsJSKey,
	}
}

// NATS is a throwaway NATS server with JetStream and the real
// services/nats/nats.conf (and the bff_clinical user it includes). The URLs
// carry each user's test credentials.
type NATS struct {
	Addr           string // host:port, no credentials
	GatewayURL     string // the core service
	PipelineURL    string // the pipeline worker
	EchoURL        string // the echo worker
	BFFClinicalURL string // the clinical app's BFF
	BFFPlatformURL string // the platform app's BFF
	EMRURL         string // the emr service

	c testcontainers.Container
}

// NATSConfDir is services/nats, where nats.conf and its include live.
func NATSConfDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "nats")
}

// NewNATS starts the container. The config is copied in rather than
// bind-mounted, so it works when the tests themselves run in a container.
// The emr user is on (emr.conf), so its rights can be tested.
func NewNATS(ctx context.Context) (n *NATS, stop func(), err error) {
	dir := NATSConfDir()
	return startNATS(ctx, filepath.Join(dir, "nats.conf"), NATSEnv(),
		map[string]string{filepath.Join(dir, "bff-clinical.test.conf"): "/etc/nats/bff-clinical.test.conf",
			filepath.Join(dir, "emr.conf"): "/etc/nats/emr.conf"})
}

// NewNATSEMROff is NewNATS as dev compose runs it by default: the emr user
// off (emr-off.conf) and no NATS_EMR_PASSWORD in the environment.
func NewNATSEMROff(ctx context.Context) (n *NATS, stop func(), err error) {
	dir := NATSConfDir()
	env := NATSEnv()
	delete(env, "NATS_EMR_PASSWORD")
	return startNATS(ctx, filepath.Join(dir, "nats.conf"), env,
		map[string]string{filepath.Join(dir, "bff-clinical.test.conf"): "/etc/nats/bff-clinical.test.conf",
			filepath.Join(dir, "emr-off.conf"): "/etc/nats/emr.conf"})
}

// StoreFilesContaining lists every file under the server's JetStream store
// (store_dir /data) that holds needle as plain bytes, for the
// encryption-at-rest test. Empty means no file does.
func (n *NATS) StoreFilesContaining(ctx context.Context, needle string) ([]string, error) {
	// The needle is an argument, not part of the script, so no quoting
	// can change what is searched for.
	out, code, err := n.Run(ctx, `grep -r -l -F -a -e "$1" /data`, needle)
	if err != nil {
		return nil, err
	}
	files := strings.Fields(out)
	switch {
	case code == 0 && len(files) > 0:
		return files, nil
	case code == 1 && len(files) == 0:
		return nil, nil
	}
	return nil, fmt.Errorf("grep exit %d: %q", code, out)
}

// StoreStreamBlocks lists the message block files (msgs/*.blk) the
// JetStream store holds for stream name, with their sizes: the positive
// control for StoreFilesContaining (the stream is on disk, under /data,
// and has written blocks).
func (n *NATS) StoreStreamBlocks(ctx context.Context, name string) (map[string]int64, error) {
	out, code, err := n.Run(ctx, `for f in /data/jetstream/*/streams/"$1"/msgs/*.blk; do [ -f "$f" ] && echo "$(wc -c < "$f") $f"; done; true`, name)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("listing blocks: exit %d: %q", code, out)
	}
	blocks := map[string]int64{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		size, file, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("listing blocks: %q", line)
		}
		b, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("listing blocks: %q", line)
		}
		blocks[file] = b
	}
	return blocks, nil
}

// execStatusPrefix marks the line Run appends with the script's exit status.
const execStatusPrefix = "__mhai_exit_status="

// Run runs script with sh in the server's container (args are $1, $2, ...)
// and returns its output (stdout and stderr) and exit status once it has
// finished.
//
// The status is read from the output, not from testcontainers' Exec: Exec
// takes the exit code from a docker exec inspect that can run before the
// daemon has marked the process as running, and then reports 0 for a
// command that has not finished (about 1 call in 1200 on the build
// server). Multiplexed reads the output to its end, which comes only when
// the process exits, so a status printed as the last line is the real one.
func (n *NATS) Run(ctx context.Context, script string, args ...string) (out string, code int, err error) {
	// The status goes on a line of its own, even after output with no
	// final newline.
	cmd := append([]string{"sh", "-c", "(\n" + script + "\n) 2>&1; printf '\\n" + execStatusPrefix + "%s\\n' \"$?\"", "sh"}, args...)
	_, r, err := n.c.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", 0, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	s := strings.TrimSuffix(string(b), "\n")
	i := strings.LastIndex(s, "\n"+execStatusPrefix)
	if i < 0 {
		return "", 0, fmt.Errorf("no exit status in the output: %q", s)
	}
	code, err = strconv.Atoi(s[i+1+len(execStatusPrefix):])
	if err != nil {
		return "", 0, fmt.Errorf("exit status in %q: %w", s[i+1:], err)
	}
	return s[:i], code, nil
}

// NATSNkey is a throwaway server with one NKey user allowed to publish
// inference.v1.> only, for testing the production authentication path.
type NATSNkey struct {
	URL      string // no credentials
	SeedPath string // the user's seed file
}

// NewNATSNkey starts a server whose only user authenticates by NKey.
func NewNATSNkey(ctx context.Context, dir string) (n *NATSNkey, stop func(), err error) {
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, nil, err
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return nil, nil, err
	}
	seed, err := kp.Seed()
	if err != nil {
		return nil, nil, err
	}
	conf := filepath.Join(dir, "nkey.conf")
	body := fmt.Sprintf(`port: 4222
jetstream { store_dir: /data }
authorization { users = [ { nkey: %s, permissions: { publish: { allow: ["inference.v1.>"] }, subscribe: { allow: ["_INBOX_gateway.>"] } } } ] }
`, pub)
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		return nil, nil, err
	}
	seedPath := filepath.Join(dir, "user.seed")
	if err := os.WriteFile(seedPath, seed, 0o600); err != nil {
		return nil, nil, err
	}
	srv, stop, err := startNATS(ctx, conf, nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return &NATSNkey{URL: "nats://" + srv.Addr, SeedPath: seedPath}, stop, nil
}

// NATSTLS is a throwaway server that requires TLS with client
// certificates (verify on, as production), with one password user,
// `gateway`. The files are PEM, written to the test's directory.
type NATSTLS struct {
	URL      string // nats://gateway:<pw>@host:port
	CAPath   string // the CA that signed the server and client certificates
	CertPath string // a client certificate signed by CAPath
	KeyPath  string // its key
	OtherCA  string // an unrelated CA (the server's certificate doesn't chain to it)
}

// NewNATSTLS generates a CA, a server certificate for localhost and a
// client certificate, and starts a server that requires them.
func NewNATSTLS(ctx context.Context, dir string) (n *NATSTLS, stop func(), err error) {
	caCert, caKey, err := writeCA(dir, "ca")
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := writeCA(dir, "other-ca"); err != nil {
		return nil, nil, err
	}
	if err := writeLeaf(dir, "server", caCert, caKey, true); err != nil {
		return nil, nil, err
	}
	if err := writeLeaf(dir, "client", caCert, caKey, false); err != nil {
		return nil, nil, err
	}
	conf := filepath.Join(dir, "tls.conf")
	body := `port: 4222
tls {
  cert_file: /etc/nats/tls/server.pem
  key_file:  /etc/nats/tls/server-key.pem
  ca_file:   /etc/nats/tls/ca.pem
  verify:    true
}
authorization { users = [ { user: gateway, password: $NATS_GATEWAY_PASSWORD, permissions: { publish: { allow: ["inference.v1.>"] }, subscribe: { allow: ["_INBOX_gateway.>"] } } } ] }
`
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		return nil, nil, err
	}
	files := map[string]string{}
	for _, f := range []string{"server.pem", "server-key.pem", "ca.pem"} {
		files[filepath.Join(dir, f)] = "/etc/nats/tls/" + f
	}
	srv, stop, err := startNATS(ctx, conf, map[string]string{"NATS_GATEWAY_PASSWORD": natsGatewayPassword}, files)
	if err != nil {
		return nil, nil, err
	}
	return &NATSTLS{
		URL:      srv.GatewayURL,
		CAPath:   filepath.Join(dir, "ca.pem"),
		CertPath: filepath.Join(dir, "client.pem"),
		KeyPath:  filepath.Join(dir, "client-key.pem"),
		OtherCA:  filepath.Join(dir, "other-ca.pem"),
	}, stop, nil
}

func writeCA(dir, name string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "mhai test " + name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(filepath.Join(dir, name+".pem"), "CERTIFICATE", der); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func writeLeaf(dir, name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, server bool) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "mhai test " + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err := writePEM(filepath.Join(dir, name+".pem"), "CERTIFICATE", der); err != nil {
		return err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writePEM(filepath.Join(dir, name+"-key.pem"), "PRIVATE KEY", kder)
}

func writePEM(path, typ string, der []byte) error {
	// 0644: the server's key is copied into a throwaway container whose
	// nats process doesn't run as the file's owner.
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o644)
}

// startNATS runs conf as /etc/nats/nats.conf with env, plus extra files
// (host path -> container path).
func startNATS(ctx context.Context, conf string, env map[string]string, extra map[string]string) (n *NATS, stop func(), err error) {
	files := []testcontainers.ContainerFile{{
		HostFilePath:      conf,
		ContainerFilePath: "/etc/nats/nats.conf",
		FileMode:          0o644,
	}}
	for host, ctr := range extra {
		files = append(files, testcontainers.ContainerFile{HostFilePath: host, ContainerFilePath: ctr, FileMode: 0o644})
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        NATSImage,
			ExposedPorts: []string{"4222/tcp"},
			Cmd:          []string{"-c", "/etc/nats/nats.conf"},
			Env:          env,
			Files:        files,
			WaitingFor:   wait.ForLog("Server is ready").WithStartupTimeout(60 * time.Second),
		},
	})
	if err != nil {
		if c != nil {
			_ = testcontainers.TerminateContainer(c)
		}
		return nil, nil, err
	}
	stop = func() { _ = testcontainers.TerminateContainer(c) }
	host, err := c.Host(ctx)
	if err != nil {
		stop()
		return nil, nil, err
	}
	port, err := c.MappedPort(ctx, "4222/tcp")
	if err != nil {
		stop()
		return nil, nil, err
	}
	addr := fmt.Sprintf("%s:%s", host, port.Port())
	u := func(user, pw string) string { return fmt.Sprintf("nats://%s:%s@%s", user, pw, addr) }
	return &NATS{
		Addr:           addr,
		GatewayURL:     u("gateway", natsGatewayPassword),
		PipelineURL:    u("pipeline", natsPipelinePassword),
		EchoURL:        u("echo", natsEchoPassword),
		BFFClinicalURL: u("bff_clinical", natsBFFClinicalPassword),
		BFFPlatformURL: u("bff_platform", natsBFFPlatformPassword),
		EMRURL:         u("emr", natsEMRPassword),
		c:              c,
	}, stop, nil
}
