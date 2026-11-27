// Package hl7bridge is the on-site HL7 bridge (design docs/designs/
// 2026-11-27-hl7v2-file-connector-and-bridge.md §4): an MLLP receiver on
// the hospital's network that acknowledges a message only once it is on
// disk, a spool of hl7v2_file batch files (internal/hl7v2 defines the
// format, the connector reads it), and a read-and-move-only SFTP server the
// emr service pulls them from. Our side stays outbound only.
//
// Nothing here logs a message, a segment, a control id, a name or a key:
// events are fixed strings with counts and closed codes, and the remote
// address of a refused connection.
package hl7bridge

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
)

// MLLP framing bytes.
const (
	startBlock = 0x0B
	endBlock   = 0x1C
	carriage   = 0x0D
)

// Framer errors (fixed values).
var (
	// ErrFrameTooLarge: a frame passed the cap; what was read so far
	// (Frame.Head) may still answer it, then the connection closes.
	ErrFrameTooLarge = errors.New("hl7bridge: frame too large")
	// ErrFraming: a start block inside a frame, or an end block not
	// followed by a carriage return. The stream can't be trusted after it.
	ErrFraming = errors.New("hl7bridge: framing error")
)

// Framer reads MLLP frames: 0x0B message 0x1C 0x0D.
type Framer struct {
	r   *bufio.Reader
	max int
	// Discarded counts bytes outside any frame (skipped, never read).
	Discarded int
	// OnStart and OnEnd are called when a frame starts and ends (the
	// receiver's per-frame deadline).
	OnStart, OnEnd func()
}

// NewFramer reads frames of at most max bytes from r.
func NewFramer(r io.Reader, max int) *Framer {
	return &Framer{r: bufio.NewReaderSize(r, 64<<10), max: max}
}

// Next returns the next frame's content. On ErrFrameTooLarge or
// ErrFraming it returns what it had read (at most max bytes) so the caller
// can try to answer from its header.
func (f *Framer) Next() ([]byte, error) {
	for {
		b, err := f.r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == startBlock {
			break
		}
		f.Discarded++
	}
	if f.OnStart != nil {
		f.OnStart()
	}
	if f.OnEnd != nil {
		defer f.OnEnd()
	}
	var buf []byte
	for {
		b, err := f.r.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, io.ErrUnexpectedEOF
			}
			return buf, err
		}
		switch b {
		case startBlock:
			return buf, ErrFraming
		case endBlock:
			n, err := f.r.ReadByte()
			if err != nil {
				return buf, io.ErrUnexpectedEOF
			}
			if n != carriage {
				return buf, ErrFraming
			}
			return buf, nil
		}
		if len(buf) >= f.max {
			return buf, ErrFrameTooLarge
		}
		buf = append(buf, b)
	}
}

// Frame wraps a message for MLLP.
func Frame(msg []byte) []byte {
	out := make([]byte, 0, len(msg)+3)
	out = append(out, startBlock)
	out = append(out, msg...)
	return append(out, endBlock, carriage)
}

// Acknowledgement codes (original mode).
const (
	AckAccept = "AA"
	AckError  = "AE"
	AckReject = "AR"
)

// HL7 table 0357 error codes the bridge uses.
const (
	errSequence    = "100" // segment sequence error: the shape
	errRequired    = "101" // required field missing: the header
	errDataType    = "102" // data type error: the encoding
	errVersion     = "203" // unsupported version id
	errApplication = "207" // application internal error
)

// echo is a header value safe to send back: unescaped, printable ASCII
// only, at most 64 characters.
func echo(m *hl7v2.Message, f, c int) string {
	v, err := m.Value(m.MSH(), f, 0, c, 1)
	if err != nil {
		return ""
	}
	v = strings.Map(func(r rune) rune {
		if r > 0x7e || r < 0x20 || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, v)
	if len(v) > 64 {
		v = v[:64]
	}
	return v
}

// Ack is the acknowledgement of the message whose header is m: sending and
// receiving application and facility swapped, MSA-2 the message's control
// id, in its own delimiters and version. errCode (HL7 table 0357) adds an
// ERR segment with that code only, never content.
func Ack(m *hl7v2.Message, code, errCode, ctrl string, at time.Time) []byte {
	d := m.D
	esc := func(s string) string { return hl7v2.Escape(s, d) }
	sep := string(d.Field)
	comp := string(d.Comp)
	version := echo(m, 12, 1)
	if !strings.HasPrefix(version, "2.") {
		version = "2.5.1"
	}
	proc := echo(m, 11, 1)
	if proc == "" {
		proc = "P"
	}
	event := echo(m, 9, 2)
	msh := strings.Join([]string{"MSH", d.Encoding(), esc(echo(m, 5, 1)), esc(echo(m, 6, 1)), esc(echo(m, 3, 1)),
		esc(echo(m, 4, 1)), hl7v2.FormatTime(at), "", "ACK" + comp + esc(event) + comp + "ACK", esc(ctrl), esc(proc),
		esc(version)}, sep)
	out := msh + "\r" + "MSA" + sep + code + sep + esc(echo(m, 10, 1)) + "\r"
	if errCode != "" {
		out += "ERR" + sep + sep + sep + errCode + comp + comp + "HL70357" + sep + "E\r"
	}
	return []byte(out)
}
