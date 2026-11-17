// Package clamd scans bytes with ClamAV's clamd (documents design §5 step
// 3, §5.2, §5.3): INSTREAM on the bytes as received, before any parser
// sees them, and VERSION for the signature database's age. Signatures
// older than 48 hours, or a clamd that doesn't answer, are not ready: the
// scan refuses to run and the document waits (D8), never passes.
//
// The client keeps nothing from a reply but a verdict: never a signature
// name (it would say what was in a patient's file) or a byte of the file.
package clamd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"
)

// MaxSignatureAge is §5.2's: older signatures refuse every scan.
const MaxSignatureAge = 48 * time.Hour

// chunk is the INSTREAM chunk size; maxReply bounds what is read back.
const (
	chunk    = 64 << 10
	maxReply = 4096
)

var (
	// ErrUnavailable: clamd didn't answer, or answered with an error.
	ErrUnavailable = errors.New("clamd: unavailable")
	// ErrStale: the signatures are older than MaxSignatureAge, or their
	// date can't be read.
	ErrStale = errors.New("clamd: signatures are stale")
)

// Client talks to one clamd over TCP.
type Client struct {
	// Addr is host:port (compose: clamav:3310, on the internal network).
	Addr string
	// Timeout bounds one command (a minute when zero).
	Timeout time.Duration
	// MaxAge is MaxSignatureAge when zero (tests may only lower it... or
	// raise it for an image whose bundled signatures are old).
	MaxAge time.Duration
	// Now is nil for time.Now.
	Now func() time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	if c == nil || c.Addr == "" {
		return nil, ErrUnavailable
	}
	t := c.Timeout
	if t == 0 {
		t = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, ErrUnavailable
	}
	dl, _ := ctx.Deadline()
	if err := conn.SetDeadline(dl); err != nil {
		_ = conn.Close()
		return nil, ErrUnavailable
	}
	return conn, nil
}

func readReply(conn net.Conn) (string, error) {
	r := bufio.NewReaderSize(conn, maxReply)
	b, err := r.ReadSlice(0)
	if err != nil {
		return "", ErrUnavailable
	}
	return string(bytes.TrimRight(b, "\x00")), nil
}

// SignatureTime asks VERSION and returns the signature database's date.
// clamd answers "ClamAV <engine>/<db version>/<db date>".
func (c *Client) SignatureTime(ctx context.Context) (time.Time, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("zVERSION\x00")); err != nil {
		return time.Time{}, ErrUnavailable
	}
	rep, err := readReply(conn)
	if err != nil {
		return time.Time{}, err
	}
	return ParseVersion(rep)
}

// ParseVersion reads the database date from a VERSION reply. A reply
// without one (clamd loaded no database) is ErrStale.
func ParseVersion(rep string) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(rep), "/")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "ClamAV ") {
		return time.Time{}, ErrStale
	}
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(parts[2]), " "))
	if err != nil {
		// clamd pads the day with a space; strings.Fields took it out.
		t, err = time.Parse("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(parts[2]), " "))
	}
	if err != nil {
		return time.Time{}, ErrStale
	}
	return t, nil
}

// Ready reports nil when clamd answers and its signatures are at most
// MaxAge old (§5.2: checked at every scan).
func (c *Client) Ready(ctx context.Context) error {
	t, err := c.SignatureTime(ctx)
	if err != nil {
		return err
	}
	max := c.MaxAge
	if max == 0 {
		max = MaxSignatureAge
	}
	if c.now().Sub(t) > max {
		return ErrStale
	}
	return nil
}

// Scan streams data to clamd (INSTREAM) and reports whether it is
// infected. A signature hit is infected; so is a limit clamd reports as
// exceeded (§5 step 3: "a limit exceeded is malware"; clamd.conf has
// AlertExceedsMax yes, so a file past MaxFileSize or MaxScanSize is a
// Heuristics.Limits.Exceeded hit). Any other reply is ErrUnavailable.
func (c *Client) Scan(ctx context.Context, data []byte) (bool, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	w := bufio.NewWriterSize(conn, chunk+4)
	if _, err := w.WriteString("zINSTREAM\x00"); err != nil {
		return false, ErrUnavailable
	}
	var size [4]byte
	for len(data) > 0 {
		n := min(len(data), chunk)
		binary.BigEndian.PutUint32(size[:], uint32(n))
		if _, err := w.Write(size[:]); err != nil {
			return false, ErrUnavailable
		}
		if _, err := w.Write(data[:n]); err != nil {
			return false, ErrUnavailable
		}
		data = data[n:]
	}
	binary.BigEndian.PutUint32(size[:], 0)
	if _, err := w.Write(size[:]); err != nil {
		return false, ErrUnavailable
	}
	if err := w.Flush(); err != nil {
		return false, ErrUnavailable
	}
	rep, err := readReply(conn)
	if err != nil {
		return false, err
	}
	return Verdict(rep)
}

// Verdict reads an INSTREAM reply.
func Verdict(rep string) (bool, error) {
	switch {
	case rep == "stream: OK":
		return false, nil
	case strings.HasPrefix(rep, "stream: ") && strings.HasSuffix(rep, " FOUND"):
		return true, nil
	case strings.Contains(rep, "size limit exceeded"):
		return true, nil
	}
	return false, ErrUnavailable
}
