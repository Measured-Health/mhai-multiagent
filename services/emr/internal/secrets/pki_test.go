package secrets

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pki is a throwaway CA, a server certificate (localhost, 127.0.0.1 and
// extra names) and a client certificate, made at run time; nothing of it
// is a literal.
type pki struct {
	dir                                    string
	CA, ServerCert, ServerKey              string
	ClientCert, ClientKey                  string
	OtherCA, OtherClientCert, OtherClientK string
}

func writePEM(t *testing.T, path, typ string, der []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode); err != nil {
		t.Fatal(err)
	}
}

func newCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, k
}

func leaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, server bool, dns []string,
	certPath, keyPath string, keyMode os.FileMode) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, DNSNames: dns}
	if server {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, certPath, "CERTIFICATE", der, 0o644)
	kd, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", kd, keyMode)
}

// newPKI writes the files into a new directory. keyMode is the server
// key's mode (0444 when a container user other than ours reads it).
func newPKI(t *testing.T, keyMode os.FileMode, serverNames ...string) pki {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := pki{dir: d, CA: filepath.Join(d, "ca.pem"), ServerCert: filepath.Join(d, "server.pem"),
		ServerKey: filepath.Join(d, "server-key.pem"), ClientCert: filepath.Join(d, "emr.pem"),
		ClientKey: filepath.Join(d, "emr-key.pem"), OtherCA: filepath.Join(d, "other-ca.pem"),
		OtherClientCert: filepath.Join(d, "other.pem"), OtherClientK: filepath.Join(d, "other-key.pem")}
	ca, caKey := newCA(t, "test CA")
	writePEM(t, p.CA, "CERTIFICATE", ca.Raw, 0o644)
	leaf(t, ca, caKey, "openbao", true, append([]string{"localhost", "openbao"}, serverNames...), p.ServerCert, p.ServerKey, keyMode)
	leaf(t, ca, caKey, "emr", false, []string{"emr"}, p.ClientCert, p.ClientKey, 0o600)
	other, otherKey := newCA(t, "other CA")
	writePEM(t, p.OtherCA, "CERTIFICATE", other.Raw, 0o644)
	leaf(t, other, otherKey, "emr", false, []string{"emr"}, p.OtherClientCert, p.OtherClientK, 0o600)
	return p
}

// randomID is a run-time value for a role or secret id: never a literal.
func randomID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return new(big.Int).SetBytes(b).Text(36)
}

// writeCreds writes an AppRole credentials file (0400 by default).
func writeCreds(t *testing.T, roleID, secretID string, mode os.FileMode) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "openbao-emr.json")
	if err := os.WriteFile(f, []byte(`{"role_id":"`+roleID+`","secret_id":"`+secretID+`"}`), mode); err != nil {
		t.Fatal(err)
	}
	return f
}
