package main

// emr synth-fhir: the synthetic FHIR R4 server (EMR plan Task 25), for the
// end-to-end run on the build server only. It refuses to start unless
// APP_ENV is development. It makes a throwaway CA and a server certificate
// for --name, and a synthetic client key pair, and writes the CA
// certificate (ca.pem) and the client's private key (client.key) to --out
// for the test that drives it; it serves synthetic patients only.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/synth"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

var errSynthEnv = errors.New("synth-fhir is for development only: set APP_ENV=development (never on a deployed host)")

func synthFHIR(args []string, getenv func(string) string, out io.Writer) error {
	if getenv("APP_ENV") != "development" {
		return errSynthEnv
	}
	fs := flag.NewFlagSet("synth-fhir", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", ":8443", "listen address")
	seed := fs.Int64("seed", 1, "generator seed")
	n := fs.Int("patients", 20, "how many synthetic patients")
	name := fs.String("name", "", "the server's DNS name (in its certificate and links)")
	dir := fs.String("out", "", "where to write ca.pem and client.key")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errUsage
	}
	if *dir == "" || *n < 1 || *n > 1000 {
		return errUsage
	}
	if _, _, err := emrurl.CheckURL("https://"+*name, []string{"https"}, false); err != nil {
		return errors.New("synth-fhir: --name must be a DNS name the connection's checks accept")
	}
	_, port, err := net.SplitHostPort(*addr)
	if err != nil {
		return errUsage
	}
	public := "https://" + *name
	if port != "443" {
		public += ":" + port
	}
	caPEM, cert, err := synthCerts(*name)
	if err != nil {
		return err
	}
	key, pub, err := synth.ClientKey()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "ca.pem"), caPEM, 0o644); err != nil {
		return errors.New("synth-fhir: cannot write ca.pem")
	}
	if err := os.WriteFile(filepath.Join(*dir, "client.key"), key, 0o600); err != nil {
		return errors.New("synth-fhir: cannot write client.key")
	}
	h := synth.Handler(synth.Generate(*seed, *n), synth.Options{BaseURL: public + "/r4", TokenURL: public + "/token", Pub: pub})
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	fmt.Fprintf(out, "synth-fhir: serving %d synthetic patients at %s/r4\n", *n, public)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("synth-fhir: cannot serve")
	}
	return nil
}

// synthCerts makes a throwaway CA and a server certificate for name,
// valid for a day.
func synthCerts(name string) ([]byte, tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "MHAI synthetic test CA (not trusted anywhere)"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, MaxPathLenZero: true}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, tls.Certificate{}, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return caPEM, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, nil
}
