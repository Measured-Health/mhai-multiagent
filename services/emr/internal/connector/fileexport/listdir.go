package fileexport

// The folder listing, read a page at a time (security review, 14 Nov).
// pkg/sftp's ReadDir builds the whole listing before it returns, so a
// hostile or broken server could list without end and exhaust memory
// before any cap applied. This is the listing alone, in SFTP version 3 on
// its own channel of the same SSH connection: OPENDIR, then READDIR page
// by page, counting entries as they arrive and stopping (errListing) as
// soon as the count passes MaxListed; every packet's length is checked
// before it is read, so no answer can be larger than MaxPacket. Only the
// name, the kind (regular file or folder) and the size are kept.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"golang.org/x/crypto/ssh"
)

// MaxPacket is the largest SFTP packet the listing reads.
const MaxPacket = 256 << 10

// dirEntry is one folder entry.
type dirEntry struct {
	name         string
	regular, dir bool
	size         int64
}

// errListing: the listing can't be read as given (too many entries, a
// packet too large or malformed, an error status). A fixed value.
var errListing = errors.New("fileexport: listing refused")

// SFTP v3 packet types and attribute flags.
const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpClose    = 4
	fxpOpendir  = 11
	fxpReaddir  = 12
	fxpStatus   = 101
	fxpHandle   = 102
	fxpName     = 104
	fxStatusEOF = 1

	attrSize        = 0x1
	attrUIDGID      = 0x2
	attrPermissions = 0x4
	attrACModTime   = 0x8
	attrExtended    = 0x80000000

	modeType    = 0o170000
	modeRegular = 0o100000
	modeDir     = 0o040000
)

type listing struct {
	r  *bufio.Reader
	w  io.Writer
	id uint32
}

func (l *listing) send(typ byte, parts ...[]byte) error {
	n := 1
	for _, p := range parts {
		n += len(p)
	}
	b := binary.BigEndian.AppendUint32(make([]byte, 0, 4+n), uint32(n))
	b = append(b, typ)
	for _, p := range parts {
		b = append(b, p...)
	}
	_, err := l.w.Write(b)
	return err
}

// recv reads one packet, refusing a length over MaxPacket before reading it.
func (l *listing) recv() (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(l.r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[:4])
	if n < 1 || n > MaxPacket {
		return 0, nil, errListing
	}
	b := make([]byte, n-1)
	if _, err := io.ReadFull(l.r, b); err != nil {
		return 0, nil, err
	}
	return h[4], b, nil
}

func (l *listing) next() []byte {
	l.id++
	return binary.BigEndian.AppendUint32(nil, l.id)
}

func sftpString(s string) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(s))), s...)
}

// reader takes values off a packet; any short read sets bad.
type reader struct {
	b   []byte
	bad bool
}

func (r *reader) u32() uint32 {
	if len(r.b) < 4 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) u64() uint64 {
	if len(r.b) < 8 {
		r.bad = true
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *reader) str() string {
	n := r.u32()
	if r.bad || uint64(n) > uint64(len(r.b)) {
		r.bad = true
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

// expect reads the answer to request id: a packet of want, or a status.
func (l *listing) expect(id []byte, want byte) (*reader, uint32, error) {
	typ, b, err := l.recv()
	if err != nil {
		return nil, 0, err
	}
	r := &reader{b: b}
	if got := r.u32(); r.bad || got != binary.BigEndian.Uint32(id) {
		return nil, 0, errListing
	}
	switch typ {
	case want:
		return r, 0, nil
	case fxpStatus:
		code := r.u32()
		if r.bad {
			return nil, 0, errListing
		}
		return nil, code, nil
	}
	return nil, 0, errListing
}

// listDir lists folder over its own SFTP channel of cl, at most MaxListed
// entries ("." and ".." aside).
func listDir(ctx context.Context, cl *ssh.Client, folder string) ([]dirEntry, error) {
	sess, err := cl.NewSession()
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stop()
	w, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := sess.RequestSubsystem("sftp"); err != nil {
		return nil, err
	}
	l := &listing{r: bufio.NewReaderSize(out, 32<<10), w: w}
	if err := l.send(fxpInit, binary.BigEndian.AppendUint32(nil, 3)); err != nil {
		return nil, err
	}
	typ, b, err := l.recv()
	if err != nil {
		return nil, err
	}
	if v := (&reader{b: b}); typ != fxpVersion || v.u32() != 3 || v.bad {
		return nil, errListing
	}
	id := l.next()
	if err := l.send(fxpOpendir, id, sftpString(folder)); err != nil {
		return nil, err
	}
	r, _, err := l.expect(id, fxpHandle)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errListing // no such folder, or not readable
	}
	handle := r.str()
	if r.bad || len(handle) > 256 {
		return nil, errListing
	}
	defer func() {
		// Best effort: the session closes either way.
		_ = l.send(fxpClose, l.next(), sftpString(handle))
	}()
	var entries []dirEntry
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		id := l.next()
		if err := l.send(fxpReaddir, id, sftpString(handle)); err != nil {
			return nil, err
		}
		r, code, err := l.expect(id, fxpName)
		if err != nil {
			return nil, err
		}
		if r == nil {
			if code == fxStatusEOF {
				return entries, nil
			}
			return nil, errListing
		}
		count := r.u32()
		if r.bad || count > MaxPacket/13 { // the smallest entry is 13 bytes on the wire
			return nil, errListing
		}
		for range count {
			e, ok := readEntry(r)
			if !ok {
				return nil, errListing
			}
			if e.name == "." || e.name == ".." {
				continue
			}
			if len(entries) == MaxListed {
				return nil, errListing
			}
			entries = append(entries, e)
		}
	}
}

// readEntry reads one name entry: filename, longname (dropped), attrs.
func readEntry(r *reader) (dirEntry, bool) {
	var e dirEntry
	name := r.str()
	_ = r.str()
	flags := r.u32()
	if flags&attrSize != 0 {
		e.size = int64(r.u64() & (1<<63 - 1))
	}
	if flags&attrUIDGID != 0 {
		r.u32()
		r.u32()
	}
	if flags&attrPermissions != 0 {
		mode := r.u32()
		e.regular = mode&modeType == modeRegular
		e.dir = mode&modeType == modeDir
	}
	if flags&attrACModTime != 0 {
		r.u32()
		r.u32()
	}
	if flags&attrExtended != 0 {
		n := r.u32()
		if n > 64 {
			return e, false
		}
		for range n {
			r.str()
			r.str()
		}
	}
	if len(name) > 255 {
		e.regular, e.dir = false, false // never read; counted as skipped
		name = ""
	}
	e.name = name
	return e, !r.bad
}
