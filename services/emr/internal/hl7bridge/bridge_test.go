package hl7bridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector/fileexport"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

func newSpool(t *testing.T) *Spool {
	t.Helper()
	d := t.TempDir()
	s := &Spool{Root: filepath.Join(d, "spool"), State: filepath.Join(d, "state")}
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func keyOf(t *testing.T, b []byte) Key {
	t.Helper()
	m, err := hl7v2.ParseMessage(b)
	if err != nil {
		t.Fatal(err)
	}
	return KeyOf(m)
}

func closedFiles(t *testing.T, s *Spool) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(s.Root, Folder, "hl7-*.hl7"))
	return m
}

// A file the spool closes is one the connector reads: the envelope's
// counts add up and every message is there.
func TestSpoolWritesTheConnectorsFormat(t *testing.T) {
	s := newSpool(t)
	for _, c := range []string{"C1", "C2", "C3"} {
		if dup, err := s.Append(oru(c), keyOf(t, oru(c))); dup || err != nil {
			t.Fatal(dup, err)
		}
	}
	if dup, err := s.Append(oru("C2"), keyOf(t, oru("C2"))); !dup || err != nil {
		t.Fatal("a duplicate stored twice")
	}
	if len(closedFiles(t, s)) != 0 {
		t.Fatal("closed early")
	}
	if err := s.Rotate(0); err != nil {
		t.Fatal(err)
	}
	files := closedFiles(t, s)
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	data, _ := os.ReadFile(files[0])
	b, err := hl7v2.ParseBatch(data)
	if err != nil || len(b.Messages) != 3 || !hl7v2.Complete(data) {
		t.Fatalf("batch: %v", err)
	}
	if _, _, ok := hl7v2.ParseFileName(filepath.Base(files[0])); !ok {
		t.Fatal("name")
	}
	if fi, _ := os.Stat(files[0]); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(s.Root, Folder)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", fi.Mode())
	}
	// The log holds digests only.
	lg, _ := os.ReadFile(filepath.Join(s.State, logName))
	if bytes.Contains(lg, []byte("C1")) || bytes.Contains(lg, []byte(hl7test.Family)) {
		t.Fatal("the log holds a value")
	}
}

// Crash safety: a write that can't be synced is never acknowledged, and
// the spool refuses until restarted; a tail written but not logged (never
// acknowledged) is cut at start.
func TestSpoolCrashSafety(t *testing.T) {
	s := newSpool(t)
	if _, err := s.Append(oru("C1"), keyOf(t, oru("C1"))); err != nil {
		t.Fatal(err)
	}
	s.Sync = func(*os.File) error { return errors.New("disk") }
	if _, err := s.Append(oru("C2"), keyOf(t, oru("C2"))); !errors.Is(err, ErrSpool) || !s.Broken() {
		t.Fatalf("a failed sync: %v", err)
	}
	if _, err := s.Append(oru("C3"), keyOf(t, oru("C3"))); !errors.Is(err, ErrSpool) {
		t.Fatal("broken spool wrote")
	}
	// Simulate the crash: an unlogged tail after C1, then restart.
	parts, _ := filepath.Glob(filepath.Join(s.Root, Folder, "*.part"))
	if len(parts) != 1 {
		t.Fatalf("parts %v", parts)
	}
	f, _ := os.OpenFile(parts[0], os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write([]byte("MSH|^~\\&|HALF-WRITTEN"))
	_ = f.Close()
	s2 := &Spool{Root: s.Root, State: s.State}
	if err := s2.Open(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	files := closedFiles(t, s2)
	if len(files) != 1 {
		t.Fatalf("recovered %v", files)
	}
	data, _ := os.ReadFile(files[0])
	b, err := hl7v2.ParseBatch(data)
	if err != nil || len(b.Messages) != 1 || b.Messages[0].ControlID() != "C1" {
		t.Fatalf("recovered batch: %v", err)
	}
	// The duplicate window survives the restart.
	if !s2.Seen(keyOf(t, oru("C1"))) {
		t.Fatal("C1 forgotten")
	}
	// A part file with no acknowledged message is dropped.
	empty := filepath.Join(s.Root, Folder, hl7v2.FileName(at, 9)+partSuffix)
	_ = os.WriteFile(empty, []byte(hl7v2.FileHeader("x", at)), 0o600)
	s3 := &Spool{Root: s.Root, State: s.State}
	if err := s3.Open(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("an empty part file kept")
	}
}

func TestSpoolRotatesAtTheCaps(t *testing.T) {
	s := newSpool(t)
	now := at
	s.Now = func() time.Time { return now }
	_, _ = s.Append(oru("C1"), keyOf(t, oru("C1")))
	if err := s.Rotate(time.Minute); err != nil || len(closedFiles(t, s)) != 0 {
		t.Fatal("rotated before its time")
	}
	now = now.Add(2 * time.Minute)
	if err := s.Rotate(time.Minute); err != nil || len(closedFiles(t, s)) != 1 {
		t.Fatal("not rotated")
	}
	// An empty spool rotates nothing.
	if err := s.Rotate(0); err != nil || len(closedFiles(t, s)) != 1 {
		t.Fatal("empty rotate")
	}
}

func TestPruneOnlyOldProcessed(t *testing.T) {
	s := newSpool(t)
	old := filepath.Join(s.Root, Folder, Processed, hl7v2.FileName(at, 1))
	recent := filepath.Join(s.Root, Folder, Processed, hl7v2.FileName(at, 2))
	rejected := filepath.Join(s.Root, Folder, Rejected, hl7v2.FileName(at, 3))
	waiting := filepath.Join(s.Root, Folder, hl7v2.FileName(at, 4))
	for _, p := range []string{old, recent, rejected, waiting} {
		_ = os.WriteFile(p, []byte("x"), 0o600)
		_ = os.Chtimes(p, time.Now().Add(-30*24*time.Hour), time.Now().Add(-30*24*time.Hour))
	}
	_ = os.Chtimes(recent, time.Now(), time.Now())
	if n := s.Prune(7 * 24 * time.Hour); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	for p, want := range map[string]bool{old: false, recent: true, rejected: true, waiting: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: present %v", filepath.Base(p), err == nil)
		}
	}
}

// --- the MLLP receiver -------------------------------------------------

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func startReceiver(t *testing.T, s *Spool, allow string, lb *logBuf) (*Receiver, string) {
	t.Helper()
	al, err := ParseAllowlist(allow)
	if err != nil {
		t.Fatal(err)
	}
	r := &Receiver{Spool: s, Allow: al, Counts: &Counters{}, Log: slog.New(slog.NewJSONHandler(lb, nil))}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = r.Serve(ctx, ln) }()
	return r, ln.Addr().String()
}

type mllpClient struct {
	c  net.Conn
	fr *Framer
}

func dialMLLP(t *testing.T, addr string) *mllpClient {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return &mllpClient{c: c, fr: NewFramer(c, hl7v2.MaxMessage)}
}

// send sends msg and returns MSA-1 of the answer, or "" when the
// connection closed without one.
func (m *mllpClient) send(t *testing.T, msg []byte) string {
	t.Helper()
	if _, err := m.c.Write(Frame(msg)); err != nil {
		return ""
	}
	ack, err := m.fr.Next()
	if err != nil {
		return ""
	}
	a, err := hl7v2.ParseMessage(ack)
	if err != nil {
		t.Fatalf("ack unreadable: %v", err)
	}
	v, _ := a.V(&a.Segs[1], 1, 1)
	return v
}

func TestReceiver(t *testing.T) {
	s := newSpool(t)
	lb := &logBuf{}
	r, addr := startReceiver(t, s, "127.0.0.1/32", lb)
	c := dialMLLP(t, addr)
	if got := c.send(t, oru("SYN-CTRL-1")); got != AckAccept {
		t.Fatalf("ORU: %q", got)
	}
	if got := c.send(t, oru("SYN-CTRL-1")); got != AckAccept {
		t.Fatalf("duplicate: %q", got)
	}
	if got := c.send(t, []byte(hl7test.ADT("SYN-CTRL-2", at))); got != AckAccept {
		t.Fatalf("ADT: %q", got)
	}
	if got := c.send(t, []byte(strings.Replace(string(oru("SYN-CTRL-3")), "PID|", "pid|", 1))); got != AckError {
		t.Fatalf("malformed: %q", got)
	}
	if got := c.send(t, []byte(strings.Replace(string(oru("SYN-CTRL-4")), "|2.5.1|", "|3.0|", 1))); got != AckReject {
		t.Fatalf("version: %q", got)
	}
	if got := c.send(t, []byte(strings.Replace(string(oru("SYN-CTRL-5")), hl7test.Family, "SYN\xc3\x28", 1))); got != AckReject {
		t.Fatalf("encoding: %q", got)
	}
	if r.Counts.Accepted.Load() != 1 || r.Counts.Duplicates.Load() != 1 || r.Counts.Dropped.Load() != 1 ||
		r.Counts.Errors.Load() != 3 {
		t.Fatalf("counts %d %d %d %d", r.Counts.Accepted.Load(), r.Counts.Duplicates.Load(), r.Counts.Dropped.Load(),
			r.Counts.Errors.Load())
	}
	_ = s.Rotate(0)
	files := closedFiles(t, s)
	data, _ := os.ReadFile(files[0])
	if b, err := hl7v2.ParseBatch(data); err != nil || len(b.Messages) != 1 {
		t.Fatalf("spooled: %v", err)
	}
	// An over-cap frame: AR, then the connection closes.
	big := append(oru("SYN-CTRL-6"), bytes.Repeat([]byte("NTE|1||x\r"), hl7v2.MaxMessage/9+1)...)
	c2 := dialMLLP(t, addr)
	if got := c2.send(t, big); got != AckReject {
		t.Fatalf("over the cap: %q", got)
	}
	if got := c2.send(t, oru("SYN-CTRL-7")); got != "" {
		t.Fatal("the connection stayed open after an over-cap frame")
	}
	// Logs: no message content or control id.
	for _, v := range []string{hl7test.Family, hl7test.Given, hl7test.MRN, "SYN-CTRL", "SYN-ORD"} {
		if strings.Contains(lb.String(), v) {
			t.Errorf("logs hold %q: %s", v, lb.String())
		}
	}
}

// No ACK before the message is durable: a failing sync gets no answer and
// the connection closes.
func TestReceiverNoAckWithoutFsync(t *testing.T) {
	s := newSpool(t)
	s.Sync = func(*os.File) error { return errors.New("disk") }
	r, addr := startReceiver(t, s, "127.0.0.1", &logBuf{})
	c := dialMLLP(t, addr)
	if got := c.send(t, oru("SYN-CTRL-1")); got != "" {
		t.Fatalf("acknowledged without a durable write: %q", got)
	}
	if r.Counts.NoAck.Load() != 1 {
		t.Fatal("not counted")
	}
}

func TestReceiverAllowlist(t *testing.T) {
	s := newSpool(t)
	r, addr := startReceiver(t, s, "10.0.0.0/8", &logBuf{})
	c := dialMLLP(t, addr)
	if got := c.send(t, oru("SYN-CTRL-1")); got != "" {
		t.Fatal("an address outside the allow-list was served")
	}
	if r.Counts.Refused.Load() != 1 {
		t.Fatal("not counted")
	}
	for _, bad := range []string{"", " , ", "not-an-ip", "10.0.0.0/33"} {
		if _, err := ParseAllowlist(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// --- the SFTP server ----------------------------------------------------

type sftpFixture struct {
	s      *Spool
	srv    *SFTPServer
	addr   string
	client ssh.Signer
}

func newSFTP(t *testing.T, s *Spool) *sftpFixture {
	t.Helper()
	_, ck, _ := ed25519.GenerateKey(rand.Reader)
	return newSFTPWith(t, s, ck)
}

func newSFTPWith(t *testing.T, s *Spool, ck ed25519.PrivateKey) *sftpFixture {
	t.Helper()
	hk, err := HostKey(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := ssh.NewSignerFromKey(ck)
	al, _ := ParseAllowlist("127.0.0.1")
	srv := &SFTPServer{Spool: s, HostKey: hk, Authorized: cs.PublicKey(), User: "synthetic-mhai", Allow: al}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }()
	return &sftpFixture{s: s, srv: srv, addr: ln.Addr().String(), client: cs}
}

func (f *sftpFixture) dial(t *testing.T, user string, auth ssh.AuthMethod) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(f.srv.HostKey.PublicKey()), Timeout: 5 * time.Second})
}

func (f *sftpFixture) sftp(t *testing.T) *sftp.Client {
	t.Helper()
	c, err := f.dial(t, "synthetic-mhai", ssh.PublicKeys(f.client))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close(); _ = c.Close() })
	return sc
}

func TestSFTPSignIn(t *testing.T) {
	f := newSFTP(t, newSpool(t))
	if _, err := f.dial(t, "synthetic-mhai", ssh.Password("guess")); err == nil {
		t.Fatal("a password signed in")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	os2, _ := ssh.NewSignerFromKey(other)
	if _, err := f.dial(t, "synthetic-mhai", ssh.PublicKeys(os2)); err == nil {
		t.Fatal("another key signed in")
	}
	if _, err := f.dial(t, "root", ssh.PublicKeys(f.client)); err == nil {
		t.Fatal("another user signed in")
	}
	c, err := f.dial(t, "synthetic-mhai", ssh.PublicKeys(f.client))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Run("id"); err == nil {
		t.Fatal("exec ran")
	}
	if _, _, err := c.OpenChannel("direct-tcpip", nil); err == nil {
		t.Fatal("port forwarding opened")
	}
}

func TestSFTPOnlyReadsAndMoves(t *testing.T) {
	s := newSpool(t)
	_, _ = s.Append(oru("C1"), keyOf(t, oru("C1")))
	_ = s.Rotate(0)
	name := filepath.Base(closedFiles(t, s)[0])
	_ = os.WriteFile(filepath.Join(s.State, "secret"), []byte("state"), 0o600)
	_ = os.Symlink(filepath.Join(s.State, "secret"), filepath.Join(s.Root, Folder, hl7v2.FileName(at, 77)))
	f := newSFTP(t, s)
	c := f.sftp(t)
	fis, err := c.ReadDir("hl7")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, fi := range fis {
		names = append(names, fi.Name())
	}
	slices.Sort(names)
	if want := []string{name, Processed, Rejected}; !slices.Equal(names, want) {
		t.Fatalf("listing %v, want %v (no symlink)", names, want)
	}
	fh, err := c.Open("hl7/" + name)
	if err != nil {
		t.Fatal(err)
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(fh)
	_ = fh.Close()
	if _, err := hl7v2.ParseBatch(buf.Bytes()); err != nil {
		t.Fatalf("read: %v", err)
	}
	for what, err := range map[string]error{
		"create":         func() error { _, err := c.Create("hl7/" + hl7v2.FileName(at, 5)); return err }(),
		"remove":         c.Remove("hl7/" + name),
		"mkdir":          c.Mkdir("hl7/x"),
		"symlink":        c.Symlink("hl7/"+name, "hl7/"+hl7v2.FileName(at, 6)),
		"chmod":          c.Chmod("hl7/"+name, 0o777),
		"traversal":      func() error { _, err := c.Open("hl7/../../state/secret"); return err }(),
		"symlink read":   func() error { _, err := c.Open("hl7/" + hl7v2.FileName(at, 77)); return err }(),
		"other folder":   func() error { _, err := c.ReadDir("/etc"); return err }(),
		"rename out":     c.Rename("hl7/"+name, "hl7/"+hl7v2.FileName(at, 8)),
		"rename renamed": c.Rename("hl7/"+name, "hl7/processed/"+hl7v2.FileName(at, 8)),
	} {
		if err == nil {
			t.Errorf("%s allowed", what)
		}
	}
	if err := c.Rename("hl7/"+name, "hl7/processed/"+name); err != nil {
		t.Fatalf("the connector's move refused: %v", err)
	}
	// The posix-rename extension is the same move, never over a file.
	_ = os.WriteFile(filepath.Join(s.Root, Folder, name), []byte("again"), 0o600)
	if err := c.PosixRename("hl7/"+name, "hl7/processed/"+name); err == nil {
		t.Fatal("posix-rename moved over a file")
	}
	_ = os.Remove(filepath.Join(s.Root, Folder, name))
	// Never over a file.
	_ = os.WriteFile(filepath.Join(s.Root, Folder, name), []byte("again"), 0o600)
	if err := c.Rename("hl7/"+name, "hl7/processed/"+name); err == nil {
		t.Fatal("moved over a file")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Root, Folder, Processed, name)); bytes.Equal(b, []byte("again")) {
		t.Fatal("overwritten")
	}
	if err := c.Rename("hl7/"+name, "hl7/rejected/"+name+".123"); err != nil {
		t.Fatalf("suffixed move refused: %v", err)
	}
	if f.srv.LastPull.Load() == 0 {
		t.Fatal("no last pull time")
	}
	if b, _ := os.ReadFile(filepath.Join(s.State, "secret")); string(b) != "state" {
		t.Fatal("state touched")
	}
}

func TestHealthHoldsCountsOnly(t *testing.T) {
	s := newSpool(t)
	_, _ = s.Append(oru("SYN-CTRL-1"), keyOf(t, oru("SYN-CTRL-1")))
	_ = s.Rotate(0)
	rec := httptest.NewRecorder()
	HealthHandler(s, &Counters{}, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	var h Health
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil || h.PendingFiles != 1 || h.Status != "degraded" || !h.DiskOK {
		t.Fatalf("%s %v", rec.Body.String(), err)
	}
	for _, v := range []string{hl7test.Family, "SYN-CTRL", "hl7-"} {
		if strings.Contains(rec.Body.String(), v) {
			t.Errorf("health holds %q", v)
		}
	}
	s.Sync = func(*os.File) error { return errors.New("disk") }
	_, _ = s.Append(oru("C2"), keyOf(t, oru("C2")))
	rec = httptest.NewRecorder()
	HealthHandler(s, &Counters{}, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("failing spool: %d", rec.Code)
	}
}

func TestConfigFromEnv(t *testing.T) {
	d := t.TempDir()
	_, ck, _ := ed25519.GenerateKey(rand.Reader)
	cs, _ := ssh.NewSignerFromKey(ck)
	kf := filepath.Join(d, "authorized_key")
	_ = os.WriteFile(kf, ssh.MarshalAuthorizedKey(cs.PublicKey()), 0o600)
	env := map[string]string{"HL7_BRIDGE_MLLP_ALLOW": "10.1.2.0/24", "HL7_BRIDGE_SFTP_ALLOW": "203.0.113.7",
		"HL7_BRIDGE_SFTP_USER": "mhai", "HL7_BRIDGE_AUTHORIZED_KEY": kf}
	get := func(k string) string { return env[k] }
	c, err := ConfigFromEnv(get)
	if err != nil || c.Retain != 7*24*time.Hour || c.Rotate != time.Minute || c.HealthAddr != "127.0.0.1:8080" {
		t.Fatalf("%+v %v", c, err)
	}
	for k, v := range map[string]string{"HL7_BRIDGE_MLLP_ALLOW": "", "HL7_BRIDGE_SFTP_ALLOW": "", "HL7_BRIDGE_SFTP_USER": "-x",
		"HL7_BRIDGE_AUTHORIZED_KEY": filepath.Join(d, "missing"), "HL7_BRIDGE_RETAIN": "1h", "HL7_BRIDGE_ROTATE": "0s",
		"HL7_BRIDGE_MLLP_TLS_CERT": "/cert-without-key"} {
		old, had := env[k]
		env[k] = v
		if _, err := ConfigFromEnv(get); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		} else if strings.Contains(err.Error(), v) && v != "" {
			t.Errorf("the error names the value: %v", err)
		}
		if had {
			env[k] = old
		} else {
			delete(env, k)
		}
	}
}

// --- end to end ----------------------------------------------------------

// The bridge end to end: MLLP → the spool → the bridge's SFTP server → the
// hl7v2_file connector (its real Fetch, with its pinned host key) → raw
// records, the bridge's file moved to processed/.
func TestBridgeToConnector(t *testing.T) {
	s := newSpool(t)
	_, addr := startReceiver(t, s, "127.0.0.1", &logBuf{})
	c := dialMLLP(t, addr)
	if got := c.send(t, oru("SYN-CTRL-1")); got != AckAccept {
		t.Fatal(got)
	}
	if got := c.send(t, []byte(hl7test.RDE("SYN-CTRL-2", "SYN-RX-1", at))); got != AckAccept {
		t.Fatal(got)
	}
	if err := s.Rotate(0); err != nil {
		t.Fatal(err)
	}
	_, ck, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKey(ck, "")
	f := newSFTPWith(t, s, ck)
	conn := fileexport.NewHL7(func(ctx context.Context, cfg connector.Config, _ emrurl.Origin, fp, user string,
		auth []ssh.AuthMethod) (*ssh.Client, error) {
		return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{User: user, Auth: auth, Timeout: 5 * time.Second,
			HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
				if ssh.FingerprintSHA256(k) != fp {
					return errors.New("host key")
				}
				return nil
			}})
	})
	key := pem.EncodeToMemory(blk)
	cfg := connector.Config{ConnectionID: uuid.New(), HospitalID: uuid.New(), Type: fileexport.HL7ID,
		BaseURL: "sftp://bridge.synthetic-hospital.example", PollSeconds: 300,
		Origin: []emrurl.Origin{"sftp://bridge.synthetic-hospital.example:22"},
		Settings: map[string]string{"folder": Folder, "username": "synthetic-mhai", "expected_every_minutes": "60",
			"host_key_fingerprint": ssh.FingerprintSHA256(f.srv.HostKey.PublicKey())}}
	creds := connector.NewCreds(map[string]seal.Secret{fileexport.SecretField: seal.NewSecret(key)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if r := conn.Test(ctx, cfg, creds); !r.OK {
		t.Fatalf("test %+v", r)
	}
	sk := &memSink{}
	if _, err := conn.Fetch(ctx, cfg, creds, nil, sk); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, r := range sk.recs {
		kinds[r.Kind]++
	}
	if kinds["observation"] != 1 || kinds["medication"] != 1 || kinds["patient"] != 2 || len(sk.rejected) != 0 {
		t.Fatalf("records %v", kinds)
	}
	moved, _ := os.ReadDir(filepath.Join(s.Root, Folder, Processed))
	if len(moved) != 1 || path.Ext(moved[0].Name()) != ".hl7" || len(closedFiles(t, s)) != 0 {
		t.Fatalf("processed %v", moved)
	}
}

type memSink struct {
	recs     []connector.RawRecord
	rejected []connector.RejectedRecord
}

func (m *memSink) Put(_ context.Context, r connector.RawRecord) error {
	m.recs = append(m.recs, r)
	return nil
}

func (m *memSink) Reject(_ context.Context, r connector.RejectedRecord) error {
	m.rejected = append(m.rejected, r)
	return nil
}
