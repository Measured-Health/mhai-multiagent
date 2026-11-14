package fileexport

// The connector (plan Task 44; design §5.4, §15.4): an SFTP pull from the
// hospital's folder through egress-emr, accepting only the host key whose
// fingerprint the platform admin typed in (egress.SSH), signing in with the
// connection's key. Each run reads the files named <kind>-<time>.csv in
// export order, at most MaxFiles; a file loaded whole is moved to
// processed/, one refused whole to rejected/; neither is ever deleted by
// us. Its cursor says whether the run read everything there was (the end
// of a first sync's backfill) and when it last loaded a new file
// (staleness by the declared export interval).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/connector"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/egress"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrtypes"
	"github.com/Measured-Health/mhai-multiagent/services/gokit/emrurl"
)

// ID is the connector type.
const ID = "file_export"

// SecretField is the SSH private key.
const SecretField = "access_key"

// Run limits.
const (
	// MaxFiles is the most files one run reads; the rest wait for the next.
	MaxFiles = 20
	// MaxListed is the most folder entries a run reads; a listing longer
	// than that is refused (unreadable_data) as soon as it passes it.
	MaxListed = 10_000
	// MinExpectedMinutes and MaxExpectedMinutes bound the declared export
	// interval (settings.expected_every_minutes): five minutes to a week.
	MinExpectedMinutes = 5
	MaxExpectedMinutes = 7 * 24 * 60
	// MinRSABits is the smallest RSA key accepted.
	MinRSABits = 2048
)

// The subfolders files are moved to. The hospital creates them; the test
// checks they are there.
const (
	Processed = "processed"
	Rejected  = "rejected"
)

// Dialer opens the SSH client for a connection: egress.SSH with the emr
// service's proxy and key, the only way out.
type Dialer func(ctx context.Context, cfg connector.Config, origin emrurl.Origin, fingerprint, user string,
	auth []ssh.AuthMethod) (*ssh.Client, error)

// Metrics are counts only.
type Metrics struct {
	FilesLoaded, FilesRejected, RowsRejected, Skipped atomic.Int64
}

// FileExport is the connector.
type FileExport struct {
	Dial Dialer
	// Now is the clock (nil: time.Now).
	Now func() time.Time
	// MaxFiles overrides the default (tests).
	MaxFiles int
	Metrics  *Metrics
}

var (
	_ connector.Connector  = (*FileExport)(nil)
	_ connector.CatchUp    = (*FileExport)(nil)
	_ connector.FileSource = (*FileExport)(nil)
)

// New returns the connector.
func New(dial Dialer) *FileExport { return &FileExport{Dial: dial, Metrics: &Metrics{}} }

func (c *FileExport) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *FileExport) metrics() *Metrics {
	if c.Metrics == nil {
		c.Metrics = &Metrics{}
	}
	return c.Metrics
}

// Type is the shared table's row.
func (c *FileExport) Type() connector.TypeInfo {
	t, _ := emrtypes.Lookup(ID)
	return t
}

var (
	folderShape   = regexp.MustCompile(`^/?[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)
	userShape     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,31}$`)
	minutesShape  = regexp.MustCompile(`^[1-9][0-9]{0,5}$`)
	namespaceText = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)
)

// Validate checks the settings against the declared fields and their
// shapes: a folder path with no "." or ".." segment, a user name, a SHA256
// host key fingerprint, the format, the export interval in range.
func (c *FileExport) Validate(cfg connector.Config) error {
	if err := connector.ValidateFields(c.Type(), cfg); err != nil {
		return err
	}
	s := cfg.Settings
	if !folderShape.MatchString(s["folder"]) || slices.ContainsFunc(strings.Split(s["folder"], "/"), func(p string) bool {
		return p == "." || p == ".."
	}) {
		return connector.ErrConfig
	}
	if !userShape.MatchString(s["username"]) || !validFingerprint(s["host_key_fingerprint"]) || s["format"] != Format {
		return connector.ErrConfig
	}
	if _, ok := ExpectedMinutes(cfg); !ok {
		return connector.ErrConfig
	}
	if ns, ok := s["mrn_namespace"]; ok && !namespaceText.MatchString(ns) {
		return connector.ErrConfig
	}
	if !strings.HasPrefix(cfg.BaseURL, "sftp://") {
		return connector.ErrConfig
	}
	return nil
}

// ExpectedMinutes is the declared export interval, when it is a whole
// number of minutes in range.
func ExpectedMinutes(cfg connector.Config) (int, bool) {
	v := cfg.Settings["expected_every_minutes"]
	if !minutesShape.MatchString(v) {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil && n >= MinExpectedMinutes && n <= MaxExpectedMinutes
}

// validFingerprint: "SHA256:" and the unpadded base64 of 32 bytes.
func validFingerprint(fp string) bool {
	rest, ok := strings.CutPrefix(fp, "SHA256:")
	if !ok {
		return false
	}
	b, err := base64.RawStdEncoding.DecodeString(rest)
	return err == nil && len(b) == 32
}

// RecordLink: an export has no record page to link to.
func (c *FileExport) RecordLink(connector.Config, string) (string, bool) { return "", false }

// signer reads the connection's private key: an unencrypted OpenSSH or PEM
// key, Ed25519, ECDSA or RSA of at least MinRSABits. Anything else is
// sign_in_refused (a passphrase-protected key cannot be used: there is no
// passphrase field).
func signer(creds connector.Creds) (ssh.Signer, bool) {
	k := creds.Get(SecretField).Bytes()
	if connector.Placeholder(k) {
		return nil, false
	}
	s, err := ssh.ParsePrivateKey(k)
	if err != nil {
		return nil, false
	}
	switch pk := s.PublicKey(); pk.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return s, true
	case ssh.KeyAlgoRSA:
		if cp, ok := pk.(ssh.CryptoPublicKey); ok {
			if rk, ok := cp.CryptoPublicKey().(interface{ Size() int }); ok && rk.Size()*8 >= MinRSABits {
				return s, true
			}
		}
	}
	return nil, false
}

// session is an open SFTP session.
type session struct {
	ssh  *ssh.Client
	sftp *sftp.Client
}

func (s *session) close() {
	if s.sftp != nil {
		_ = s.sftp.Close()
	}
	if s.ssh != nil {
		_ = s.ssh.Close()
	}
}

// open signs in through egress-emr. Errors are closed codes.
func (c *FileExport) open(ctx context.Context, cfg connector.Config, creds connector.Creds) (*session, error) {
	if c.Dial == nil {
		return nil, connector.Fail("unreachable", 0)
	}
	var origin emrurl.Origin
	for _, o := range cfg.Origin {
		if strings.HasPrefix(string(o), "sftp://") {
			if origin != "" {
				return nil, connector.Fail("sign_in_refused", 0)
			}
			origin = o
		}
	}
	if origin == "" || len(cfg.Origin) != 1 {
		return nil, connector.Fail("sign_in_refused", 0) // nothing sent
	}
	sg, ok := signer(creds)
	if !ok {
		return nil, connector.Fail("sign_in_refused", 0)
	}
	cl, err := c.Dial(ctx, cfg, origin, cfg.Settings["host_key_fingerprint"], cfg.Settings["username"],
		[]ssh.AuthMethod{ssh.PublicKeys(sg)})
	switch {
	case err == nil:
	case errors.Is(err, egress.ErrHostKey), errors.Is(err, egress.ErrSignIn), errors.Is(err, egress.ErrOrigin):
		return nil, connector.Fail("sign_in_refused", 0)
	case ctx.Err() != nil:
		return nil, connector.Fail("timed_out", 0)
	default:
		return nil, connector.Fail("unreachable", 0)
	}
	sc, err := sftp.NewClient(cl)
	if err != nil {
		_ = cl.Close()
		return nil, connector.Fail("unreachable", 0)
	}
	return &session{ssh: cl, sftp: sc}, nil
}

// remoteError is a failed SFTP call's closed code.
func remoteError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return connector.Fail("timed_out", 0)
	case errors.Is(err, os.ErrNotExist), errors.Is(err, os.ErrPermission):
		return connector.Fail("unreadable_data", 0)
	}
	return connector.Fail("unreachable", 0)
}

// Test signs in and checks the folder and its two subfolders are there.
// It reads no file and moves nothing.
func (c *FileExport) Test(ctx context.Context, cfg connector.Config, creds connector.Creds) connector.TestResult {
	if c.Validate(cfg) != nil {
		return connector.Result("unreadable_data")
	}
	s, err := c.open(ctx, cfg, creds)
	if err != nil {
		return connector.Result(connector.CodeOf(err))
	}
	defer s.close()
	folder := cfg.Settings["folder"]
	for _, p := range []string{folder, path.Join(folder, Processed), path.Join(folder, Rejected)} {
		fi, err := s.sftp.Stat(p)
		if err != nil {
			return connector.Result(connector.CodeOf(remoteError(ctx, err)))
		}
		if !fi.IsDir() {
			return connector.Result("unreadable_data")
		}
	}
	return connector.Result("ok")
}

// cursor is the connector's position: no position in the folder is kept
// (moved files are the position), only whether the last run read
// everything, when a new file was last loaded, and how many files it left
// unloaded on purpose.
type cursor struct {
	V        int    `json:"v"`
	Drained  bool   `json:"d,omitempty"`
	NewFile  string `json:"nf,omitempty"`
	SetAside int    `json:"sa,omitempty"`
}

const cursorVersion = 1

func decodeCursor(raw connector.Cursor) (cursor, bool) {
	var c cursor
	if len(raw) == 0 || len(raw) > 256 || json.Unmarshal(raw, &c) != nil || c.V != cursorVersion || c.SetAside < 0 {
		return cursor{}, false
	}
	return c, true
}

func (c cursor) encode() connector.Cursor {
	b, _ := json.Marshal(c)
	return b
}

// CaughtUp reports whether the run that wrote cur read every file there
// was (a file refused and moved aside counts as read).
func (c *FileExport) CaughtUp(raw connector.Cursor) bool {
	cur, ok := decodeCursor(raw)
	return ok && cur.Drained
}

// NewFileAt is when the run that wrote cur loaded its last new file.
func (c *FileExport) NewFileAt(raw connector.Cursor) (time.Time, bool) {
	cur, ok := decodeCursor(raw)
	if !ok || cur.NewFile == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, cur.NewFile)
	return t.UTC(), err == nil
}

// SetAside is how many files the run that wrote cur left unloaded on
// purpose: refused whole and moved to rejected/, misnamed, or apparently
// still being written. The scheduler reports them (an unreadable_data
// error row) without failing the run.
func (c *FileExport) SetAside(raw connector.Cursor) int {
	cur, ok := decodeCursor(raw)
	if !ok {
		return 0
	}
	return cur.SetAside
}

// inProgress: a name a hospital's export uses while it writes (a .part
// suffix, a leading dot): never read, and not reported.
func inProgress(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp")
}

// Fetch reads the folder's files in export order, at most MaxFiles. A file
// is loaded and moved to processed/, or refused whole and moved to
// rejected/, or left in place when it looks half-written; the last two are
// counted as set aside, and the run goes on. A failure of ours or the
// server's (storing, reading, a move that can't be made) stops the run and
// leaves the file in place to be read again (its rows already stored are
// no-ops then).
func (c *FileExport) Fetch(ctx context.Context, cfg connector.Config, creds connector.Creds, _ connector.Cursor,
	sk connector.RawSink) (connector.Cursor, error) {
	out := cursor{V: cursorVersion}
	if c.Validate(cfg) != nil {
		return out.encode(), connector.Fail("unreadable_data", 0)
	}
	rj, ok := sk.(connector.Rejecter)
	if !ok {
		return out.encode(), connector.Fail("unreachable", 0) // no dead letter: store nothing
	}
	s, err := c.open(ctx, cfg, creds)
	if err != nil {
		return out.encode(), err
	}
	defer s.close()
	folder := cfg.Settings["folder"]
	// Read a page at a time and capped as it goes (listdir.go): never the
	// whole listing first.
	entries, err := listDir(ctx, s.ssh, folder)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return out.encode(), connector.Fail("timed_out", 0)
	case errors.Is(err, errListing):
		return out.encode(), connector.Fail("unreadable_data", 0)
	default:
		return out.encode(), connector.Fail("unreachable", 0)
	}
	m := c.metrics()
	var files []fileKey
	for _, e := range entries {
		if e.dir || inProgress(e.name) {
			continue
		}
		k, ok := parseName(e.name)
		if !ok || !e.regular {
			// Misnamed (or not a plain file): never read or moved, but
			// reported, so a results file named wrongly isn't silent.
			m.Skipped.Add(1)
			out.SetAside++
			continue
		}
		files = append(files, k)
	}
	slices.SortFunc(files, fileKey.less)
	limit := MaxFiles
	if c.MaxFiles > 0 {
		limit = c.MaxFiles
	}
	out.Drained = len(files) <= limit
	if len(files) > limit {
		files = files[:limit]
	}
	for _, f := range files {
		if ctx.Err() != nil {
			out.Drained = false
			return out.encode(), connector.Fail("timed_out", 0)
		}
		res, err := c.load(ctx, s.sftp, cfg, folder, f, sk, rj)
		if err != nil {
			out.Drained = false
			return out.encode(), err
		}
		switch res {
		case loaded:
			out.NewFile = c.now().Format(time.RFC3339Nano)
		case setAside:
			out.SetAside++
		}
	}
	return out.encode(), nil
}

type loadResult int

const (
	loaded loadResult = iota
	setAside
)

// move puts the file at p into dir under its own name, or, when that name
// is taken (a re-export under the same name), under its name with a
// suffix: never over another file.
func (c *FileExport) move(sc *sftp.Client, folder, dir, name string) error {
	p := path.Join(folder, name)
	if err := sc.Rename(p, path.Join(folder, dir, name)); err == nil {
		return nil
	}
	return sc.Rename(p, path.Join(folder, dir, name+"."+strconv.FormatInt(c.now().UnixNano(), 10)))
}

// load reads one file.
func (c *FileExport) load(ctx context.Context, sc *sftp.Client, cfg connector.Config, folder string, f fileKey,
	sk connector.RawSink, rj connector.Rejecter) (loadResult, error) {
	m := c.metrics()
	p := path.Join(folder, f.name)
	moveErr := func(error) error {
		if ctx.Err() != nil {
			return connector.Fail("timed_out", 0)
		}
		return connector.Fail("unreadable_data", 0) // the folder's configuration: visible, never silent
	}
	refuse := func() (loadResult, error) {
		if err := c.move(sc, folder, Rejected, f.name); err != nil {
			return 0, moveErr(err)
		}
		m.FilesRejected.Add(1)
		return setAside, nil
	}
	fi, err := sc.Lstat(p)
	if err != nil {
		return 0, remoteError(ctx, err)
	}
	if !fi.Mode().IsRegular() {
		return setAside, nil
	}
	if fi.Size() > MaxFile {
		return refuse()
	}
	fh, err := sc.Open(p)
	if err != nil {
		return 0, remoteError(ctx, err)
	}
	data, err := io.ReadAll(io.LimitReader(fh, MaxFile+1))
	_ = fh.Close()
	if err != nil {
		return 0, remoteError(ctx, err)
	}
	if len(data) > MaxFile {
		return refuse()
	}
	// A file shorter than listed, or one that doesn't end a row, may still
	// be being written: left in place for the next run, and reported.
	if int64(len(data)) != fi.Size() || (len(data) > 0 && data[len(data)-1] != '\n') {
		return setAside, nil
	}
	ns := cfg.Settings["mrn_namespace"]
	// The whole file first: a file refused whole stores nothing.
	for _, err := range ParseBytes(f.kind, data, ns) {
		if err != nil {
			return refuse()
		}
	}
	for row, err := range ParseBytes(f.kind, data, ns) {
		if err != nil {
			return refuse() // not reached: the same bytes parsed whole above
		}
		if row.Reject != nil {
			m.RowsRejected.Add(1)
			if err := rj.Reject(ctx, connector.RejectedRecord{Kind: row.Reject.Kind, SourceID: row.Reject.SourceID,
				SourceVersion: row.Reject.SourceVersion, SourceUpdatedAt: row.Reject.SourceUpdatedAt}); err != nil {
				return 0, err
			}
			continue
		}
		for _, r := range row.Records {
			if err := sk.Put(ctx, r); err != nil {
				return 0, err
			}
		}
	}
	if err := c.move(sc, folder, Processed, f.name); err != nil {
		return 0, moveErr(err)
	}
	m.FilesLoaded.Add(1)
	return loaded, nil
}
