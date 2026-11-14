package fileexport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// Synthetic test proxy key, not a secret.
var proxyKey = []byte("0123456789abcdef0123456789abcdef")

const (
	sftpHost = "sftp.synthetic-hospital.example"
	folder   = "/outbound/mhai"
	user     = "synthetic-export"
)

// server is an in-process SFTP server (in memory) behind a fake egress
// proxy that routes the connection's host to it, and records each CONNECT.
type server struct {
	t        *testing.T
	hostKey  ssh.Signer
	allowed  ssh.PublicKey
	clientPK []byte // the connection's private key, OpenSSH PEM
	addr     string
	proxy    *url.URL
	mu       sync.Mutex
	connects []string
	dials    atomic.Int64
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{t: t}
	_, hk, _ := ed25519.GenerateKey(rand.Reader)
	s.hostKey, _ = ssh.NewSignerFromKey(hk)
	_, ck, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(ck, "")
	if err != nil {
		t.Fatal(err)
	}
	s.clientPK = pem.EncodeToMemory(blk)
	cs, _ := ssh.NewSignerFromKey(ck)
	s.allowed = cs.PublicKey()
	handlers := sftp.InMemHandler()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(s.allowed.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("refused")
	}}
	cfg.AddHostKey(s.hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s.addr = ln.Addr().String()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSFTP(c, cfg, handlers)
		}
	}()
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.connects = append(s.connects, r.Host)
		s.mu.Unlock()
		if r.Method != http.MethodConnect || r.Host != sftpHost+":22" || r.Header.Get("Proxy-Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		up, err := net.Dial("tcp", s.addr)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = up.Close()
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(up, brw); _ = up.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
	}))
	t.Cleanup(p.Close)
	s.proxy, _ = url.Parse(p.URL)
	s.mkdir(path.Dir(folder))
	s.mkdir(folder)
	s.mkdir(path.Join(folder, Processed))
	s.mkdir(path.Join(folder, Rejected))
	return s
}

func serveSFTP(c net.Conn, cfg *ssh.ServerConfig, h sftp.Handlers) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range in {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if ok {
					rs := sftp.NewRequestServer(ch, h)
					_ = rs.Serve()
					_ = rs.Close()
				}
			}
		}()
	}
}

// direct is an SFTP client straight to the server (the test's own hand,
// not the connector's).
func (s *server) direct() *sftp.Client {
	s.t.Helper()
	sg, err := ssh.ParsePrivateKey(s.clientPK)
	if err != nil {
		s.t.Fatal(err)
	}
	c, err := ssh.Dial("tcp", s.addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(sg)},
		HostKeyCallback: ssh.FixedHostKey(s.hostKey.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		s.t.Fatal(err)
	}
	sc, err := sftp.NewClient(c)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = sc.Close(); _ = c.Close() })
	return sc
}

func (s *server) mkdir(p string) {
	s.t.Helper()
	if err := s.direct().Mkdir(p); err != nil {
		s.t.Fatal(err)
	}
}

func (s *server) put(name string, data []byte) {
	s.t.Helper()
	f, err := s.direct().Create(path.Join(folder, name))
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		s.t.Fatal(err)
	}
	_ = f.Close()
}

func (s *server) list(dir string) []string {
	s.t.Helper()
	fis, err := s.direct().ReadDir(path.Join(folder, dir))
	if err != nil {
		s.t.Fatal(err)
	}
	var out []string
	for _, fi := range fis {
		if !fi.IsDir() {
			out = append(out, fi.Name())
		}
	}
	slices.Sort(out)
	return out
}

func (s *server) read(p string) []byte {
	s.t.Helper()
	f, err := s.direct().Open(path.Join(folder, p))
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(f)
	return b
}

func (s *server) config() connector.Config {
	return connector.Config{ConnectionID: uuid.New(), HospitalID: uuid.New(), Type: ID, BaseURL: "sftp://" + sftpHost,
		Origin: []emrurl.Origin{emrurl.Origin("sftp://" + sftpHost + ":22")}, PollSeconds: 300,
		Settings: map[string]string{"folder": folder, "username": user, "format": Format, "expected_every_minutes": "60",
			"host_key_fingerprint": ssh.FingerprintSHA256(s.hostKey.PublicKey()), "mrn_namespace": testNS}}
}

func (s *server) creds() connector.Creds {
	return connector.NewCreds(map[string]seal.Secret{SecretField: seal.NewSecret(s.clientPK)})
}

func (s *server) connector() *FileExport {
	c := New(func(ctx context.Context, cfg connector.Config, o emrurl.Origin, fp, u string, a []ssh.AuthMethod) (*ssh.Client, error) {
		s.dials.Add(1)
		return egress.SSH(ctx, cfg, s.proxy, proxyKey, o, fp, u, a)
	})
	return c
}

// memSink records what a fetch stores and rejects.
type memSink struct {
	mu       sync.Mutex
	recs     []connector.RawRecord
	rejected []connector.RejectedRecord
	failPut  error
}

func (m *memSink) Put(_ context.Context, r connector.RawRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut != nil {
		return m.failPut
	}
	m.recs = append(m.recs, r)
	return nil
}

func (m *memSink) Reject(_ context.Context, r connector.RejectedRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rejected = append(m.rejected, r)
	return nil
}

// putOnly is a sink that keeps no dead letters.
type putOnly struct{ recs []connector.RawRecord }

func (p *putOnly) Put(_ context.Context, r connector.RawRecord) error {
	p.recs = append(p.recs, r)
	return nil
}

func samples(t *testing.T) map[string][]byte {
	t.Helper()
	files, _ := filepath.Glob("testdata/sample/*.csv")
	out := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = b
	}
	return out
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// A file loaded whole is moved to processed/, byte for byte, never
// deleted; the records reach the sink in export order, patients first; the
// run read everything and says when it loaded a new file.
func TestProcessedMoveNotDelete(t *testing.T) {
	s := newServer(t)
	sm := samples(t)
	for n, b := range sm {
		s.put(n, b)
	}
	c := s.connector()
	sk := &memSink{}
	cur, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.list(""); len(got) != 0 {
		t.Fatalf("left in the folder: %v", got)
	}
	got := s.list(Processed)
	if len(got) != len(sm) {
		t.Fatalf("processed: %v", got)
	}
	for _, n := range got {
		if string(s.read(path.Join(Processed, n))) != string(sm[n]) {
			t.Errorf("%s changed when moved", n)
		}
	}
	if len(sk.recs) != 13 || len(sk.rejected) != 0 {
		t.Fatalf("%d records, %d rejected", len(sk.recs), len(sk.rejected))
	}
	var kinds []string
	for _, r := range sk.recs {
		if len(kinds) == 0 || kinds[len(kinds)-1] != r.Kind {
			kinds = append(kinds, r.Kind)
		}
	}
	if !slices.Equal(kinds, []string{"patient", "location", "encounter", "observation", "medication"}) {
		t.Errorf("order %v", kinds)
	}
	if !c.CaughtUp(cur) {
		t.Error("not caught up after reading every file")
	}
	if at, ok := c.NewFileAt(cur); !ok || time.Since(at) > time.Minute {
		t.Errorf("new file at %v %v", at, ok)
	}
	// Nothing new: nothing read, no new file.
	cur, err = c.Fetch(ctx(t), s.config(), s.creds(), cur, sk)
	if err != nil || len(sk.recs) != 13 {
		t.Fatalf("second run: %v, %d", err, len(sk.recs))
	}
	if _, ok := c.NewFileAt(cur); ok || !c.CaughtUp(cur) {
		t.Error("an empty run reported a new file, or not caught up")
	}
	if c.Metrics.FilesLoaded.Load() != 5 {
		t.Errorf("files loaded %d", c.Metrics.FilesLoaded.Load())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.connects {
		if h != sftpHost+":22" {
			t.Errorf("CONNECT %s", h)
		}
	}
}

// A29 (SFTP): only the host key the platform admin typed in is accepted,
// at test and at every sync: another key is sign_in_refused and nothing is
// read or moved.
func TestWrongHostKeyRefusedAtTestAndSync(t *testing.T) {
	s := newServer(t)
	s.put("results-20261114T060000Z.csv", samples(t)["results-20261114T060000Z.csv"])
	cfg := s.config()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	osg, _ := ssh.NewSignerFromKey(other)
	cfg.Settings["host_key_fingerprint"] = ssh.FingerprintSHA256(osg.PublicKey())
	c := s.connector()
	if r := c.Test(ctx(t), cfg, s.creds()); r.OK || r.Code != "sign_in_refused" {
		t.Fatalf("test: %+v", r)
	}
	sk := &memSink{}
	if _, err := c.Fetch(ctx(t), cfg, s.creds(), nil, sk); connector.CodeOf(err) != "sign_in_refused" {
		t.Fatalf("fetch: %v", err)
	}
	if len(sk.recs) != 0 || len(s.list("")) != 1 || len(s.list(Processed)) != 0 {
		t.Fatal("something read or moved behind the wrong host key")
	}
	if r := c.Test(ctx(t), s.config(), s.creds()); !r.OK {
		t.Fatalf("the right key: %+v", r)
	}
}

// A key the server refuses, a placeholder, a malformed or passphrase-
// protected key, a weak RSA key: sign_in_refused. A key we can't use is
// never sent: nothing is dialled.
func TestKeyRefusedSignIn(t *testing.T) {
	s := newServer(t)
	c := s.connector()
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKey(other, "")
	if r := c.Test(ctx(t), s.config(), connector.NewCreds(map[string]seal.Secret{SecretField: seal.NewSecret(pem.EncodeToMemory(blk))})); r.Code != "sign_in_refused" {
		t.Fatalf("refused key: %+v", r)
	}
	before := s.dials.Load()
	enc, _ := ssh.MarshalPrivateKeyWithPassphrase(other, "", []byte("synthetic-passphrase"))
	for name, k := range map[string][]byte{"placeholder": []byte("changeme"), "junk": []byte("not a key"),
		"passphrase": pem.EncodeToMemory(enc), "empty": nil} {
		if r := c.Test(ctx(t), s.config(), connector.NewCreds(map[string]seal.Secret{SecretField: seal.NewSecret(k)})); r.Code != "sign_in_refused" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if s.dials.Load() != before {
		t.Error("an unusable key was dialled with")
	}
	// An origin that isn't one sftp origin: nothing is sent.
	cfg := s.config()
	cfg.Origin = append(cfg.Origin, "sftp://other.synthetic-hospital.example:22")
	if r := c.Test(ctx(t), cfg, s.creds()); r.Code != "sign_in_refused" || s.dials.Load() != before {
		t.Errorf("two origins: %+v", r)
	}
}

// A file refused whole stores nothing and is moved to rejected/ (never
// deleted); the other files load, and the run ends unreadable_data.
func TestBadFileMovedToRejected(t *testing.T) {
	s := newServer(t)
	sm := samples(t)
	bad := strings.Replace(string(sm["results-20261114T060000Z.csv"]), "local_code", "code", 1)
	s.put("results-20261114T060000Z.csv", []byte(bad))
	s.put("vitals-20261114T060000Z.csv", sm["vitals-20261114T060000Z.csv"])
	c := s.connector()
	sk := &memSink{}
	cur, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk)
	if err != nil {
		t.Fatalf("a refused file moved aside is handled: %v", err)
	}
	if got := s.list(Rejected); !slices.Equal(got, []string{"results-20261114T060000Z.csv"}) {
		t.Fatalf("rejected %v", got)
	}
	if string(s.read(path.Join(Rejected, "results-20261114T060000Z.csv"))) != bad {
		t.Error("the refused file changed")
	}
	if got := s.list(Processed); !slices.Equal(got, []string{"vitals-20261114T060000Z.csv"}) {
		t.Fatalf("processed %v", got)
	}
	for _, r := range sk.recs {
		if strings.HasPrefix(r.SourceID, "SYNR-") {
			t.Fatalf("a record of the refused file was stored: %s", r.SourceID)
		}
	}
	// Handled, so a first sync can still catch up (and go live), and the
	// refusal is reported for the run's error row.
	if len(sk.recs) != 2 || !c.CaughtUp(cur) || c.SetAside(cur) != 1 || c.Metrics.FilesRejected.Load() != 1 {
		t.Fatalf("%d records, caught up %v", len(sk.recs), c.CaughtUp(cur))
	}
	if _, ok := c.NewFileAt(cur); !ok {
		t.Error("the loaded file isn't counted as new")
	}
	// An over-large file is refused without being read.
	s.put("results-20261114T070000Z.csv", make([]byte, MaxFile+1))
	if cur, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk); err != nil || c.SetAside(cur) != 1 {
		t.Fatalf("large: %v", err)
	}
	if got := s.list(Rejected); len(got) != 2 {
		t.Fatalf("rejected %v", got)
	}
}

// A row the format doesn't allow is a dead letter (its identity only); the
// rest of the file loads and the file is processed.
func TestDeadLetterRowsLoadRest(t *testing.T) {
	s := newServer(t)
	data := header("results") + resultRow(nil) + resultRow(map[string]string{"source_result_id": "SYNR-T-2",
		"collected_at": "2026-11-14T05:10:00"})
	s.put("results-20261114T060000Z.csv", []byte(data))
	c := s.connector()
	sk := &memSink{}
	if _, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk); err != nil {
		t.Fatal(err)
	}
	if len(sk.recs) != 1 || len(sk.rejected) != 1 || sk.rejected[0].SourceID != "SYNR-T-2" || sk.rejected[0].Kind != "observation" {
		t.Fatalf("records %+v rejected %+v", sk.recs, sk.rejected)
	}
	if len(s.list(Processed)) != 1 || c.Metrics.RowsRejected.Load() != 1 {
		t.Fatal("file not processed")
	}
	// A sink that keeps no dead letters stores nothing and moves nothing.
	s.put("results-20261114T070000Z.csv", []byte(data))
	po := &putOnly{}
	if _, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, po); connector.CodeOf(err) != "unreachable" {
		t.Fatalf("no dead letters: %v", err)
	}
	if len(po.recs) != 0 || len(s.list("")) != 1 {
		t.Fatal("stored or moved without a dead letter")
	}
}

// A failure of ours while storing leaves the file in place to be read
// again; nothing is moved.
func TestStoreFailureLeavesFile(t *testing.T) {
	s := newServer(t)
	s.put("results-20261114T060000Z.csv", samples(t)["results-20261114T060000Z.csv"])
	c := s.connector()
	sk := &memSink{failPut: errors.New("database down")}
	cur, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk)
	if connector.CodeOf(err) != "unreachable" || len(s.list("")) != 1 || len(s.list(Processed)) != 0 || c.CaughtUp(cur) {
		t.Fatalf("err %v, folder %v", err, s.list(""))
	}
	if _, ok := c.NewFileAt(cur); ok {
		t.Error("a file not loaded counted as new")
	}
}

// At most MaxFiles a run, oldest export first; the run isn't caught up
// until a run reads everything.
func TestRunLimitsAndCatchUp(t *testing.T) {
	s := newServer(t)
	b := samples(t)["vitals-20261114T060000Z.csv"]
	for _, n := range []string{"vitals-20261114T080000Z.csv", "vitals-20261114T060000Z.csv", "vitals-20261114T070000Z.csv"} {
		s.put(n, b)
	}
	c := s.connector()
	c.MaxFiles = 2
	sk := &memSink{}
	cur, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk)
	if err != nil || c.CaughtUp(cur) {
		t.Fatalf("first: %v caught up %v", err, c.CaughtUp(cur))
	}
	if got := s.list(""); !slices.Equal(got, []string{"vitals-20261114T080000Z.csv"}) {
		t.Fatalf("left %v (want the newest)", got)
	}
	cur, err = c.Fetch(ctx(t), s.config(), s.creds(), cur, sk)
	if err != nil || !c.CaughtUp(cur) || len(s.list("")) != 0 {
		t.Fatalf("second: %v %v", err, c.CaughtUp(cur))
	}
}

// Anything that isn't <kind>-<time>.csv is left alone: never read, never
// moved, counted only.
func TestOtherFilesLeftAlone(t *testing.T) {
	s := newServer(t)
	s.put("Synthia Testperson.csv", samples(t)["results-20261114T060000Z.csv"])
	s.put("results-20261114T060000Z.csv.part", []byte("partial"))
	c := s.connector()
	sk := &memSink{}
	cur, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk)
	if err != nil || len(sk.recs) != 0 || len(s.list("")) != 2 || c.Metrics.Skipped.Load() != 1 {
		t.Fatalf("%v %d %v", err, len(sk.recs), s.list(""))
	}
	// A misnamed file is reported (no name, a count), so a lost results
	// file isn't silent; a file still being written (.part) is not.
	if c.SetAside(cur) != 1 {
		t.Errorf("set aside %d, want the misnamed file only", c.SetAside(cur))
	}
	if _, ok := c.NewFileAt(cur); ok {
		t.Error("a skipped file counted as new")
	}
}

// The test checks the folder and both subfolders exist; it reads and
// moves nothing.
func TestTestChecksFolders(t *testing.T) {
	s := newServer(t)
	s.put("results-20261114T060000Z.csv", samples(t)["results-20261114T060000Z.csv"])
	c := s.connector()
	if r := c.Test(ctx(t), s.config(), s.creds()); !r.OK {
		t.Fatalf("%+v", r)
	}
	if len(s.list("")) != 1 {
		t.Fatal("the test moved a file")
	}
	cfg := s.config()
	cfg.Settings["folder"] = "/outbound/missing"
	if r := c.Test(ctx(t), cfg, s.creds()); r.Code != "unreadable_data" {
		t.Fatalf("missing folder: %+v", r)
	}
	if err := s.direct().RemoveDirectory(path.Join(folder, Rejected)); err != nil {
		t.Fatal(err)
	}
	if r := c.Test(ctx(t), s.config(), s.creds()); r.Code != "unreadable_data" {
		t.Fatalf("no rejected/: %+v", r)
	}
}

// Settings are checked for shape before anything goes out.
func TestValidate(t *testing.T) {
	s := newServer(t)
	c := s.connector()
	if err := c.Validate(s.config()); err != nil {
		t.Fatal(err)
	}
	bad := map[string]map[string]string{
		"traversal":     {"folder": "/outbound/../etc"},
		"dot":           {"folder": "/outbound/./x"},
		"folder spaces": {"folder": "/out bound"},
		"user":          {"username": "root;ls"},
		"fingerprint":   {"host_key_fingerprint": "MD5:aa:bb"},
		"format":        {"format": "csv"},
		"interval low":  {"expected_every_minutes": "4"},
		"interval high": {"expected_every_minutes": "10081"},
		"interval junk": {"expected_every_minutes": "60m"},
		"namespace":     {"mrn_namespace": "urn x"},
		"undeclared":    {"api_key": "x"},
	}
	for name, over := range bad {
		cfg := s.config()
		for k, v := range over {
			cfg.Settings[k] = v
		}
		if c.Validate(cfg) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	cfg := s.config()
	delete(cfg.Settings, "expected_every_minutes")
	if c.Validate(cfg) == nil {
		t.Error("no interval accepted")
	}
	cfg = s.config()
	cfg.BaseURL = "https://" + sftpHost
	if c.Validate(cfg) == nil {
		t.Error("an https base address accepted")
	}
	if _, ok := c.RecordLink(cfg, "SYNP-CSV-0001"); ok {
		t.Error("a record link from an export")
	}
}

// Errors are closed codes: never a host, a folder, a file name or a cell.
func TestErrorsNameNothing(t *testing.T) {
	s := newServer(t)
	s.put("results-20261114T060000Z.csv", []byte("Synthia,Testperson\n"))
	c := s.connector()
	_, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, &memSink{})
	for _, w := range []string{sftpHost, folder, "results-2026", "Synthia", "Testperson"} {
		if err == nil || strings.Contains(err.Error(), w) {
			t.Errorf("error %v names %q", err, w)
		}
	}
	fe := &FileError{Reason: ReasonHeader, Line: 1}
	if fe.Error() != "fileexport: file refused: header" {
		t.Error(fe.Error())
	}
}

// Security review (14 Nov): a name already in processed/ (a re-export under
// the same name) is moved beside it with a suffix, never over it, so it
// can't block the files after it.
func TestNameCollisionNeverOverwrites(t *testing.T) {
	s := newServer(t)
	b := samples(t)["vitals-20261114T060000Z.csv"]
	s.put("vitals-20261114T060000Z.csv", b)
	c := s.connector()
	if _, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, &memSink{}); err != nil {
		t.Fatal(err)
	}
	again := []byte(strings.Replace(string(b), "131", "132", 1))
	s.put("vitals-20261114T060000Z.csv", again)
	s.put("vitals-20261114T070000Z.csv", b)
	if _, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, &memSink{}); err != nil {
		t.Fatal(err)
	}
	got := s.list(Processed)
	if len(got) != 3 || len(s.list("")) != 0 {
		t.Fatalf("processed %v, folder %v", got, s.list(""))
	}
	if string(s.read(path.Join(Processed, "vitals-20261114T060000Z.csv"))) != string(b) {
		t.Fatal("the first file was overwritten")
	}
}

// Residual from the review: a file that is shorter than its listed size,
// or doesn't end a row, may still be being written: it is left where it
// is (not refused, not moved), reported, and read again next run.
func TestHalfWrittenFileLeft(t *testing.T) {
	s := newServer(t)
	b := samples(t)["vitals-20261114T060000Z.csv"]
	s.put("vitals-20261114T060000Z.csv", b[:len(b)-1])
	c := s.connector()
	sk := &memSink{}
	cur, err := c.Fetch(ctx(t), s.config(), s.creds(), nil, sk)
	if err != nil || len(sk.recs) != 0 || len(s.list("")) != 1 || len(s.list(Rejected)) != 0 || c.SetAside(cur) != 1 {
		t.Fatalf("%v %d %v", err, len(sk.recs), s.list(""))
	}
}
