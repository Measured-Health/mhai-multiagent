package natstls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pki writes a throwaway CA and a client certificate it signed, made at run
// time (no key material in the repository), and returns their paths.
func pki(t *testing.T) (dir string, f Files) {
	t.Helper()
	dir = t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "emr"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, typ string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	return dir, Files{CA: write("ca.pem", "CERTIFICATE", caDER), Cert: write("emr.pem", "CERTIFICATE", der),
		Key: write("emr-key.pem", "EC PRIVATE KEY", keyDER)}
}

// The config trusts the named CA only, sends the client certificate and
// never goes below TLS 1.2.
func TestConfigTrustsTheCAOnly(t *testing.T) {
	_, f := pki(t)
	c, err := f.Config()
	if err != nil {
		t.Fatal(err)
	}
	if c.MinVersion != tls.VersionTLS12 || len(c.Certificates) != 1 || c.RootCAs == nil || c.InsecureSkipVerify {
		t.Fatalf("config: %+v", c)
	}
	sys, _ := x509.SystemCertPool()
	if sys != nil && c.RootCAs.Equal(sys) {
		t.Fatal("the system roots are trusted")
	}
}

// A missing, half-named or unreadable set is the one fixed error, which
// never names a path.
func TestConfigRefusals(t *testing.T) {
	dir, f := pki(t)
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]Files{
		"none":         {},
		"no key":       {CA: f.CA, Cert: f.Cert},
		"missing ca":   {CA: filepath.Join(dir, "nope.pem"), Cert: f.Cert, Key: f.Key},
		"ca not pem":   {CA: junk, Cert: f.Cert, Key: f.Key},
		"key not pem":  {CA: f.CA, Cert: f.Cert, Key: junk},
		"cert as key":  {CA: f.CA, Cert: f.Cert, Key: f.Cert},
		"missing cert": {CA: f.CA, Cert: filepath.Join(dir, "gone.pem"), Key: f.Key},
	} {
		_, err := bad.Config()
		if !errors.Is(err, ErrFiles) || strings.Contains(err.Error(), dir) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if (Files{}).Set() || !(Files{Key: "k"}).Set() || (Files{CA: "a", Cert: "b"}).Complete() || !f.Complete() {
		t.Fatal("Set / Complete")
	}
}
