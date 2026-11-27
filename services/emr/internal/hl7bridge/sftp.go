package hl7bridge

// The built-in SFTP server (design §4.3): public-key sign-in with one
// authorised key and one user, the sftp subsystem only, and a view of the
// spool that allows exactly what the hl7v2_file connector does: list and
// stat hl7/ and its two subfolders, read a closed file in hl7/, and move a
// file from hl7/ into processed/ or rejected/, never over another file.
// Everything else (write, create, delete, mkdir, symlinks, setstat, other
// renames) is refused. Every path is cleaned and checked against those
// shapes before it touches the disk, and no symlink is ever followed.

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
)

// SFTP limits.
const (
	MaxSessions      = 4
	SignInsPerMinute = 10
	SFTPIdle         = 10 * time.Minute
)

// movedName is a name in processed/ or rejected/: the file's own, or with
// the connector's ".<nanoseconds>" suffix when the name was taken.
var movedName = regexp.MustCompile(`^(hl7-[0-9]{8}T[0-9]{6}Z(?:-[0-9]{1,4})?\.hl7)(?:\.[0-9]{1,20})?$`)

// SFTPServer serves the spool.
type SFTPServer struct {
	Spool   *Spool
	HostKey ssh.Signer
	// Authorized is the one public key allowed; User the one user name.
	Authorized ssh.PublicKey
	User       string
	Allow      *Allowlist
	Log        *slog.Logger

	sessions atomic.Int64
	LastPull atomic.Int64 // unix seconds of the last file read
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func (s *SFTPServer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// rateOK records a sign-in attempt from ip and reports whether it is
// within SignInsPerMinute.
func (s *SFTPServer) rateOK(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == nil {
		s.attempts = map[string][]time.Time{}
	}
	now := time.Now()
	var keep []time.Time
	for _, t := range s.attempts[ip] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	s.attempts[ip] = keep
	if len(s.attempts) > 10_000 {
		s.attempts = map[string][]time.Time{ip: keep}
	}
	return len(keep) <= SignInsPerMinute
}

func (s *SFTPServer) config() *ssh.ServerConfig {
	want := s.Authorized.Marshal()
	cfg := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			okUser := subtle.ConstantTimeCompare([]byte(c.User()), []byte(s.User)) == 1
			okKey := subtle.ConstantTimeCompare(k.Marshal(), want) == 1
			if okUser && okKey {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("refused")
		},
		// No password, keyboard-interactive or "none" sign-in.
	}
	cfg.AddHostKey(s.HostKey)
	return cfg
}

// Serve accepts on ln until ctx ends.
func (s *SFTPServer) Serve(ctx context.Context, ln net.Listener) error {
	cfg := s.config()
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		ip := remoteIP(c)
		if !s.Allow.Allows(ip) {
			s.log().Warn("sftp_refused", "reason", "address", "remote", ip.String())
			_ = c.Close()
			continue
		}
		if !s.rateOK(ip.String()) {
			s.log().Warn("sftp_refused", "reason", "rate", "remote", ip.String())
			_ = c.Close()
			continue
		}
		if s.sessions.Add(1) > MaxSessions {
			s.sessions.Add(-1)
			s.log().Warn("sftp_refused", "reason", "sessions", "remote", ip.String())
			_ = c.Close()
			continue
		}
		go func() {
			defer s.sessions.Add(-1)
			s.serveConn(c, cfg)
		}()
	}
}

// idleConn closes a connection idle for SFTPIdle.
type idleConn struct{ net.Conn }

func (c idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(SFTPIdle))
	return c.Conn.Read(b)
}

func (s *SFTPServer) serveConn(c net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	sc, chans, reqs, err := ssh.NewServerConn(idleConn{c}, cfg)
	if err != nil {
		s.log().Warn("sftp_signin_refused", "remote", remoteIP(c).String())
		return
	}
	_ = c.SetDeadline(time.Time{})
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs) // tcpip-forward and the like: refused
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.Prohibited, "only sftp")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = ch.Close() }()
			for req := range in {
				ok := req.Type == "subsystem" && len(req.Payload) == 8 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if !ok {
					continue // no shell, exec, pty, env or agent
				}
				rs := sftp.NewRequestServer(ch, s.handlers())
				_ = rs.Serve()
				_ = rs.Close()
				return
			}
		}()
	}
}

// view is the spool as the SFTP client sees it.
type view struct{ s *SFTPServer }

func (s *SFTPServer) handlers() sftp.Handlers {
	v := &view{s: s}
	return sftp.Handlers{FileGet: v, FilePut: v, FileCmd: v, FileList: v}
}

var errDenied = os.ErrPermission

// place is what a cleaned path names.
type place struct {
	dir  string // "", "hl7", "hl7/processed", "hl7/rejected"
	name string // a file in dir, or ""
}

// resolve cleans p and reports what it names; anything outside the shapes
// below is refused.
func resolve(p string) (place, bool) {
	for i := 0; i < len(p); i++ {
		if p[i] == 0 {
			return place{}, false
		}
	}
	c := path.Clean("/" + p)
	switch c {
	case "/":
		return place{}, true
	case "/" + Folder:
		return place{dir: Folder}, true
	case "/" + Folder + "/" + Processed:
		return place{dir: Folder + "/" + Processed}, true
	case "/" + Folder + "/" + Rejected:
		return place{dir: Folder + "/" + Rejected}, true
	}
	dir, name := path.Split(c)
	dir = path.Clean(dir)
	switch dir {
	case "/" + Folder:
		if _, _, ok := hl7v2.ParseFileName(name); ok {
			return place{dir: Folder, name: name}, true
		}
	case "/" + Folder + "/" + Processed, "/" + Folder + "/" + Rejected:
		if movedName.MatchString(name) {
			return place{dir: dir[1:], name: name}, true
		}
	}
	return place{}, false
}

func (v *view) disk(p place) string {
	return filepath.Join(v.s.Spool.Root, filepath.FromSlash(p.dir), p.name)
}

// lstat is a regular file or one of the folders, never a symlink.
func (v *view) lstat(p place) (os.FileInfo, error) {
	if p.dir == "" && p.name == "" {
		return os.Lstat(v.s.Spool.Root)
	}
	fi, err := os.Lstat(v.disk(p))
	if err != nil {
		return nil, os.ErrNotExist
	}
	if p.name == "" && !fi.IsDir() || p.name != "" && !fi.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	return fi, nil
}

// Fileread opens a closed file in hl7/ for reading, refusing a symlink.
func (v *view) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	p, ok := resolve(r.Filepath)
	if !ok || p.dir != Folder || p.name == "" {
		return nil, errDenied
	}
	f, err := os.OpenFile(v.disk(p), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, os.ErrNotExist
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, os.ErrNotExist
	}
	v.s.LastPull.Store(time.Now().Unix())
	return f, nil
}

// Filewrite: never.
func (v *view) Filewrite(*sftp.Request) (io.WriterAt, error) { return nil, errDenied }

// Filecmd allows one thing: moving a file from hl7/ into processed/ or
// rejected/ under its own name (or the connector's suffixed one), never
// over a file. The move resets the file's time (retention starts then).
func (v *view) Filecmd(r *sftp.Request) error {
	if r.Method != "Rename" {
		return errDenied
	}
	from, ok1 := resolve(r.Filepath)
	to, ok2 := resolve(r.Target)
	if !ok1 || !ok2 || from.dir != Folder || from.name == "" || to.name == "" ||
		(to.dir != Folder+"/"+Processed && to.dir != Folder+"/"+Rejected) {
		return errDenied
	}
	if m := movedName.FindStringSubmatch(to.name); m == nil || m[1] != from.name {
		return errDenied
	}
	src, dst := v.disk(from), v.disk(to)
	if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
		return os.ErrNotExist
	}
	// Link then unlink: fails if the target exists (rename would replace it).
	if err := os.Link(src, dst); err != nil {
		return errDenied
	}
	if err := os.Remove(src); err != nil {
		_ = os.Remove(dst)
		return errDenied
	}
	now := time.Now()
	_ = os.Chtimes(dst, now, now)
	_ = syncDir(filepath.Dir(src))
	_ = syncDir(filepath.Dir(dst))
	return nil
}

type listing []os.FileInfo

func (l listing) ListAt(out []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[off:])
	if n+int(off) == len(l) {
		return n, io.EOF
	}
	return n, nil
}

// named renames a FileInfo (for "/", whose disk name is the spool root's).
type named struct {
	os.FileInfo
	name string
}

func (n named) Name() string { return n.name }

// Filelist lists a folder (regular files and the allowed folders only) or
// stats one path.
func (v *view) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	p, ok := resolve(r.Filepath)
	if !ok {
		return nil, os.ErrNotExist
	}
	switch r.Method {
	case "Stat", "Lstat":
		fi, err := v.lstat(p)
		if err != nil {
			return nil, err
		}
		name := path.Base(path.Clean("/" + r.Filepath))
		return listing{named{fi, name}}, nil
	case "List":
		if p.name != "" {
			return nil, errDenied
		}
		if p.dir == "" {
			fi, err := v.lstat(place{dir: Folder})
			if err != nil {
				return nil, err
			}
			return listing{fi}, nil
		}
		es, err := os.ReadDir(v.disk(p))
		if err != nil {
			return nil, os.ErrNotExist
		}
		var out listing
		for _, e := range es {
			if e.IsDir() {
				if p.dir == Folder && (e.Name() == Processed || e.Name() == Rejected) {
					if fi, err := e.Info(); err == nil {
						out = append(out, fi)
					}
				}
				continue
			}
			if !e.Type().IsRegular() {
				continue // symlinks and the rest: never shown
			}
			if p.dir == Folder {
				if _, _, ok := hl7v2.ParseFileName(e.Name()); !ok && !isPart(e.Name()) {
					continue
				}
			}
			if fi, err := e.Info(); err == nil {
				out = append(out, fi)
			}
		}
		return out, nil
	}
	return nil, errDenied // Readlink and anything else
}

func isPart(name string) bool {
	if len(name) <= len(partSuffix) || name[len(name)-len(partSuffix):] != partSuffix {
		return false
	}
	_, _, ok := hl7v2.ParseFileName(name[:len(name)-len(partSuffix)])
	return ok
}
