package hl7bridge

// The spool (design §4.2): one open batch file at a time, each accepted
// message appended whole and fsynced, then its digest and end offset
// appended to the duplicate log and fsynced, and only then acknowledged.
// A file is closed (BTS, FTS, fsync, rename to .hl7, fsync of the folder)
// on a timer or at a size or count cap. At start, an open file is cut back
// to the last offset the log recorded (anything after it was never
// acknowledged) and closed.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
)

// Spool limits.
const (
	// RotateMessages and RotateBytes close a file before the connector's
	// caps (20,000 messages, 50 MB) could be reached.
	RotateMessages = 10_000
	RotateBytes    = 40 << 20
	// DuplicateWindow is how long a control id is remembered.
	DuplicateWindow = 7 * 24 * time.Hour
	partSuffix      = ".part"
	logName         = "received.log"
)

// The folders under the spool root, as the SFTP server shows them.
const (
	Folder    = "hl7"
	Processed = "processed"
	Rejected  = "rejected"
)

// ErrSpool: the spool can't be written; nothing is acknowledged until it
// can (a fixed value).
var ErrSpool = errors.New("hl7bridge: spool unavailable")

// Key is a message's duplicate key: the SHA-256 of its sending
// application, facility and control id, length-prefixed. The values
// themselves are never stored.
type Key [32]byte

// KeyOf is m's duplicate key.
func KeyOf(m *hl7v2.Message) Key {
	h := sha256.New()
	for _, f := range []int{3, 4, 10} {
		v := m.Raw(m.MSH(), f, 0, 1, 1)
		fmt.Fprintf(h, "%d:%s/", len(v), v)
	}
	var k Key
	copy(k[:], h.Sum(nil))
	return k
}

// Spool writes the batch files.
type Spool struct {
	// Root holds hl7/ (and its two subfolders); State the log.
	Root, State string
	Now         func() time.Time
	// Sync is f.Sync (tests make it fail).
	Sync func(f *os.File) error

	mu        sync.Mutex
	f         *os.File
	name      string // the open file's final name (without .part)
	count     int
	size      int64
	opened    time.Time
	log       *os.File
	seen      map[Key]time.Time
	seq       int
	lastSec   int64
	broken    bool
	LastWrite time.Time
}

func (s *Spool) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Spool) sync(f *os.File) error {
	if s.Sync != nil {
		return s.Sync(f)
	}
	return f.Sync()
}

func (s *Spool) dir() string { return filepath.Join(s.Root, Folder) }

func syncDir(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// logEntry is one line of the log: digest, file, end offset, time.
type logEntry struct {
	key  Key
	file string
	end  int64
	at   time.Time
}

func parseLog(line string) (logEntry, bool) {
	p := strings.Fields(line)
	if len(p) != 4 || len(p[0]) != 64 {
		return logEntry{}, false
	}
	var e logEntry
	b, err := hex.DecodeString(p[0])
	if err != nil {
		return logEntry{}, false
	}
	copy(e.key[:], b)
	if _, _, ok := hl7v2.ParseFileName(p[1]); !ok {
		return logEntry{}, false
	}
	e.file = p[1]
	if e.end, err = strconv.ParseInt(p[2], 10, 64); err != nil || e.end < 0 {
		return logEntry{}, false
	}
	sec, err := strconv.ParseInt(p[3], 10, 64)
	if err != nil {
		return logEntry{}, false
	}
	e.at = time.Unix(sec, 0).UTC()
	return e, true
}

// Open makes the folders (0700), reads the log, recovers an open file and
// compacts the log to the duplicate window.
func (s *Spool) Open() error {
	for _, d := range []string{s.dir(), filepath.Join(s.dir(), Processed), filepath.Join(s.dir(), Rejected), s.State} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return ErrSpool
		}
	}
	s.seen = map[Key]time.Time{}
	ends := map[string]int64{}
	counts := map[string]int{}
	lp := filepath.Join(s.State, logName)
	var keep []logEntry
	if lf, err := os.Open(lp); err == nil {
		sc := bufio.NewScanner(lf)
		for sc.Scan() {
			e, ok := parseLog(sc.Text())
			if !ok {
				continue // a line cut by a crash: it was never acknowledged
			}
			if e.end > ends[e.file] {
				ends[e.file] = e.end
			}
			counts[e.file]++
			if s.now().Sub(e.at) < DuplicateWindow {
				s.seen[e.key] = e.at
				keep = append(keep, e)
			}
		}
		_ = lf.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrSpool
	}
	// Recover open files.
	parts, _ := filepath.Glob(filepath.Join(s.dir(), "hl7-*.hl7"+partSuffix))
	for _, p := range parts {
		name := strings.TrimSuffix(filepath.Base(p), partSuffix)
		if counts[name] == 0 {
			// Only a header: no message in it was ever acknowledged.
			if err := os.Remove(p); err != nil {
				return ErrSpool
			}
			continue
		}
		f, err := os.OpenFile(p, os.O_RDWR, 0o600)
		if err != nil {
			return ErrSpool
		}
		if err := f.Truncate(ends[name]); err != nil {
			_ = f.Close()
			return ErrSpool
		}
		if _, err := f.Seek(ends[name], 0); err != nil {
			_ = f.Close()
			return ErrSpool
		}
		s.f, s.name, s.count = f, name, counts[name]
		if err := s.closeFile(); err != nil {
			return err
		}
	}
	// Compact the log.
	tmp := lp + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrSpool
	}
	w := bufio.NewWriter(tf)
	for _, e := range keep {
		fmt.Fprintf(w, "%s %s %d %d\n", hex.EncodeToString(e.key[:]), e.file, e.end, e.at.Unix())
	}
	if w.Flush() != nil || s.sync(tf) != nil || tf.Close() != nil || os.Rename(tmp, lp) != nil || syncDir(s.State) != nil {
		return ErrSpool
	}
	s.log, err = os.OpenFile(lp, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return ErrSpool
	}
	return nil
}

// Seen reports whether k was stored within the duplicate window.
func (s *Spool) Seen(k Key) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.seen[k]
	return ok && s.now().Sub(at) < DuplicateWindow
}

// Broken reports whether a write failed and the spool refuses until
// restarted.
func (s *Spool) Broken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken
}

func (s *Spool) openFile() error {
	now := s.now()
	if now.Unix() == s.lastSec {
		s.seq++
	} else {
		s.seq, s.lastSec = 1, now.Unix()
	}
	name := hl7v2.FileName(now, s.seq)
	f, err := os.OpenFile(filepath.Join(s.dir(), name+partSuffix), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hdr := hl7v2.FileHeader(name, now)
	if _, err := f.WriteString(hdr); err != nil {
		_ = f.Close()
		return err
	}
	s.f, s.name, s.count, s.size, s.opened = f, name, 0, int64(len(hdr)), now
	return nil
}

// closeFile writes the trailer, syncs, and renames the open file to its
// final name. The caller holds mu (or is Open).
func (s *Spool) closeFile() error {
	if s.f == nil {
		return nil
	}
	f := s.f
	s.f = nil
	_, err := f.WriteString(hl7v2.FileTrailer(s.count))
	if err == nil {
		err = s.sync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(filepath.Join(s.dir(), s.name+partSuffix), filepath.Join(s.dir(), s.name))
	}
	if err == nil {
		err = syncDir(s.dir())
	}
	if err != nil {
		s.broken = true
		return ErrSpool
	}
	return nil
}

// Append stores msg (a frame ParseMessage read) under k and returns once
// it is durable: dup is true when k was stored already (nothing written).
// An error means nothing may be acknowledged.
func (s *Spool) Append(msg []byte, k Key) (dup bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken {
		return false, ErrSpool
	}
	if at, ok := s.seen[k]; ok && s.now().Sub(at) < DuplicateWindow {
		return true, nil
	}
	if s.f == nil {
		if err := s.openFile(); err != nil {
			s.broken = true
			return false, ErrSpool
		}
	}
	body := msg
	if n := len(body); n > 0 && body[n-1] != '\r' && body[n-1] != '\n' {
		body = append(append([]byte(nil), body...), '\r')
	}
	start := s.size
	if _, err := s.f.Write(body); err != nil || s.sync(s.f) != nil {
		// Cut back what may have been written, and refuse until restart.
		_ = s.f.Truncate(start)
		s.broken = true
		return false, ErrSpool
	}
	s.size += int64(len(body))
	now := s.now()
	line := fmt.Sprintf("%s %s %d %d\n", hex.EncodeToString(k[:]), s.name, s.size, now.Unix())
	if _, err := s.log.WriteString(line); err != nil || s.sync(s.log) != nil {
		s.broken = true
		return false, ErrSpool
	}
	s.count++
	s.seen[k] = now
	s.LastWrite = now
	if s.count >= RotateMessages || s.size >= RotateBytes {
		if err := s.closeFile(); err != nil {
			return false, nil // this message is durable; the next one fails
		}
	}
	return false, nil
}

// Rotate closes the open file if it holds a message and is older than
// every (or at once when every is 0).
func (s *Spool) Rotate(every time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil || s.count == 0 || (every > 0 && s.now().Sub(s.opened) < every) {
		return nil
	}
	return s.closeFile()
}

// Close closes the open file and the log.
func (s *Spool) Close() error {
	err := s.Rotate(0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.log != nil {
		_ = s.log.Close()
		s.log = nil
	}
	return err
}

// Counts are the files waiting in hl7/ and in rejected/ (names only, never
// opened).
func (s *Spool) Counts() (pending, rejected int) {
	count := func(dir string, pending bool) int {
		es, err := os.ReadDir(dir)
		if err != nil {
			return -1
		}
		n := 0
		for _, e := range es {
			if e.Type().IsRegular() && (!pending || !strings.HasSuffix(e.Name(), partSuffix)) {
				n++
			}
		}
		return n
	}
	return count(s.dir(), true), count(filepath.Join(s.dir(), Rejected), false)
}

// Prune deletes files in processed/ moved there longer than retain ago (the
// move resets a file's time), and nothing else. It returns how many.
func (s *Spool) Prune(retain time.Duration) int {
	dir := filepath.Join(s.dir(), Processed)
	es, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range es {
		if !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || s.now().Sub(fi.ModTime()) < retain {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}
