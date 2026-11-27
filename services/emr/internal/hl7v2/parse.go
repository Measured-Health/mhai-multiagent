// Package hl7v2 reads HL7 v2 messages in ER7 (the "pipe and hat"
// encoding), alone or in batch files, and writes the batch files the
// hl7v2_file connector reads (design docs/designs/2026-11-27-hl7v2-file-
// connector-and-bridge.md §3.3; format docs/specs/formats/hl7v2_file_v1.md).
// It is the one definition of that format: the connector reads files with
// it and the on-site bridge writes them with it.
//
// It is strict and capped: delimiters come from each message's own MSH (or
// FHS/BHS), every count is capped as it is read, values stay escaped until
// they are read (so an escaped delimiter never splits anything), and an
// encoding it can't be sure of is refused, never guessed. Nothing here
// logs; errors are closed reasons and message numbers, never content.
package hl7v2

import (
	"strings"
	"unicode/utf8"
)

// Limits (design §3.3). Anything over refuses the file (or the message).
const (
	MaxFile                = 50 << 20
	MaxMessage             = 1 << 20
	MaxSegment             = 256 << 10
	MaxMessages            = 20_000
	MaxSegmentsPerMsg      = 2_000
	MaxFields              = 256
	MaxRepetitions         = 64
	MaxComponents          = 32
	MaxSubcomponents       = 16
	MaxEscape              = 512
	defaultFieldSep   byte = '|'
)

// Error refuses a file or a message whole. Reason is one of the constants
// below; Message is the 1-based number of the message at fault (0 for the
// file or its envelope). Neither carries content.
type Error struct {
	Reason  string
	Message int
}

func (e *Error) Error() string { return "hl7v2: refused: " + e.Reason }

// The reasons.
const (
	ReasonTooLarge        = "too_large"
	ReasonMessageTooLarge = "message_too_large"
	ReasonSegmentTooLong  = "segment_too_long"
	ReasonTooMany         = "too_many_messages"
	ReasonEncoding        = "encoding"
	ReasonShape           = "shape"
	ReasonEnvelope        = "envelope"
	ReasonHeader          = "header"
)

// Delims are a message's delimiters (MSH-1, MSH-2).
type Delims struct {
	Field, Comp, Rep, Esc, Sub, Trunc byte
}

// Standard are the delimiters the bridge writes with.
var Standard = Delims{Field: '|', Comp: '^', Rep: '~', Esc: '\\', Sub: '&'}

// Encoding is MSH-2 for d.
func (d Delims) Encoding() string {
	s := string([]byte{d.Comp, d.Rep, d.Esc, d.Sub})
	if d.Trunc != 0 {
		s += string(d.Trunc)
	}
	return s
}

// Charsets read (MSH-18): what each decodes to.
const (
	CharsetASCII  = "ascii"
	CharsetUTF8   = "utf-8"
	CharsetLatin1 = "latin1"
)

// Segment is one segment: F[0] is its id. For MSH, F[1] is MSH-1 (the
// field separator) and F[2] MSH-2, so F[n] is always field n. Values are
// raw (escaped).
type Segment struct {
	ID string
	F  []string
}

// Field is field n raw, or "".
func (s *Segment) Field(n int) string {
	if n < 1 || n >= len(s.F) {
		return ""
	}
	return s.F[n]
}

// Message is one parsed message.
type Message struct {
	D       Delims
	Charset string
	Segs    []Segment
	// Text is the message's segments as read (in UTF-8), each ending "\r".
	Text string
}

// MSH is the message's header segment.
func (m *Message) MSH() *Segment { return &m.Segs[0] }

// Batch is a parsed file: its messages in order.
type Batch struct {
	Messages  []*Message
	Enveloped bool
}

func isSegID(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// punct: printable ASCII that is neither a letter, a digit nor a space.
func punct(c byte) bool {
	return c > ' ' && c < 0x7f && (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z')
}

// delimsOf reads the delimiters of a header segment line (MSH, FHS or BHS):
// the character after the id, then the encoding characters up to the next
// field separator.
func delimsOf(line string) (Delims, bool) {
	if len(line) < 8 {
		return Delims{}, false
	}
	d := Delims{Field: line[3]}
	rest := line[4:]
	enc, _, _ := strings.Cut(rest, string(d.Field))
	if len(enc) != 4 && len(enc) != 5 {
		return Delims{}, false
	}
	all := []byte{d.Field}
	for i := 0; i < len(enc); i++ {
		all = append(all, enc[i])
	}
	for i, c := range all {
		if !punct(c) {
			return Delims{}, false
		}
		for _, o := range all[:i] {
			if o == c {
				return Delims{}, false
			}
		}
	}
	d.Comp, d.Rep, d.Esc, d.Sub = enc[0], enc[1], enc[2], enc[3]
	if len(enc) == 5 {
		d.Trunc = enc[4]
	}
	return d, true
}

// splitLines splits data into segments on \r, \n or \r\n, skipping empty
// ones, refusing a segment over MaxSegment.
func splitLines(s string, yield func(line string) bool) (string, bool) {
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '\r' && s[i] != '\n' {
			if i-start >= MaxSegment {
				return ReasonSegmentTooLong, false
			}
			continue
		}
		if i > start {
			if !yield(s[start:i]) {
				return "", true
			}
		}
		start = i + 1
	}
	return "", true
}

// splitFields splits a segment line into fields with d, checking every
// count. For a header segment (MSH, FHS, BHS), F[1] is the separator and
// F[2] the encoding characters.
func splitFields(line string, d Delims, header bool) ([]string, bool) {
	var f []string
	rest := line
	if header {
		f = append(f, line[:3], string(d.Field))
		rest = line[4:]
	}
	for {
		i := strings.IndexByte(rest, d.Field)
		if i < 0 {
			f = append(f, rest)
			break
		}
		f = append(f, rest[:i])
		rest = rest[i+1:]
		if len(f) > MaxFields {
			return nil, false
		}
	}
	if len(f) > MaxFields+1 || (!header && len(f[0]) != 3) {
		return nil, false
	}
	for n, v := range f {
		if header && n <= 2 {
			continue
		}
		if n == 0 {
			continue
		}
		if !countsOK(v, d) {
			return nil, false
		}
	}
	return f, true
}

// countsOK checks one field's repetitions, components and subcomponents
// against the caps, in one pass, allocating nothing.
func countsOK(v string, d Delims) bool {
	reps, comps, subs := 1, 1, 1
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case d.Rep:
			reps, comps, subs = reps+1, 1, 1
			if reps > MaxRepetitions {
				return false
			}
		case d.Comp:
			comps, subs = comps+1, 1
			if comps > MaxComponents {
				return false
			}
		case d.Sub:
			subs++
			if subs > MaxSubcomponents {
				return false
			}
		}
	}
	return true
}

// charsetOf reads MSH-18 (raw field). "" is ok=false.
func charsetOf(raw string, d Delims) (string, bool) {
	if strings.IndexByte(raw, d.Rep) >= 0 {
		return "", false // more than one character set: ISO 2022 switching
	}
	first, _, _ := strings.Cut(raw, string(d.Comp))
	switch strings.ToUpper(strings.TrimSpace(first)) {
	case "", "ASCII":
		return CharsetASCII, true
	case "UNICODE UTF-8", "UTF-8":
		return CharsetUTF8, true
	case "8859/1":
		return CharsetLatin1, true
	}
	return "", false
}

// bytesOK checks a message's bytes are valid in its character set, with no
// control character but a tab.
func bytesOK(s, cs string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
		if c >= 0x80 && cs == CharsetASCII {
			return false
		}
		if c >= 0x80 && c < 0xa0 && cs == CharsetLatin1 {
			return false // C1 controls
		}
	}
	return cs != CharsetUTF8 || utf8.ValidString(s)
}

func latin1ToUTF8(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i := 0; i < len(s); i++ {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}

// buildMessage parses one message from its segment lines (the first is
// MSH), n its 1-based number.
func buildMessage(lines []string, n int) (*Message, *Error) {
	d, ok := delimsOf(lines[0])
	if !ok {
		return nil, &Error{Reason: ReasonHeader, Message: n}
	}
	if len(lines) > MaxSegmentsPerMsg {
		return nil, &Error{Reason: ReasonShape, Message: n}
	}
	size := 0
	for _, l := range lines {
		size += len(l) + 1
	}
	if size > MaxMessage {
		return nil, &Error{Reason: ReasonMessageTooLarge, Message: n}
	}
	mshF, ok := splitFields(lines[0], d, true)
	if !ok {
		return nil, &Error{Reason: ReasonShape, Message: n}
	}
	cs, ok := charsetOf(get(mshF, 18), d)
	if !ok {
		return nil, &Error{Reason: ReasonEncoding, Message: n}
	}
	for _, l := range lines {
		if !bytesOK(l, cs) {
			return nil, &Error{Reason: ReasonEncoding, Message: n}
		}
	}
	if cs == CharsetLatin1 {
		for i, l := range lines {
			lines[i] = latin1ToUTF8(l)
		}
		if mshF, ok = splitFields(lines[0], d, true); !ok {
			return nil, &Error{Reason: ReasonShape, Message: n}
		}
	}
	m := &Message{D: d, Charset: cs, Segs: make([]Segment, 0, len(lines))}
	m.Segs = append(m.Segs, Segment{ID: "MSH", F: mshF})
	for _, l := range lines[1:] {
		if len(l) < 3 || !isSegID(l[:3]) || (len(l) > 3 && l[3] != d.Field) {
			return nil, &Error{Reason: ReasonShape, Message: n}
		}
		switch l[:3] {
		case "MSH", "FHS", "BHS", "BTS", "FTS":
			return nil, &Error{Reason: ReasonShape, Message: n}
		}
		f, ok := splitFields(l, d, false)
		if !ok {
			return nil, &Error{Reason: ReasonShape, Message: n}
		}
		m.Segs = append(m.Segs, Segment{ID: l[:3], F: f})
	}
	if err := checkHeader(m); err != nil {
		err.Message = n
		return nil, err
	}
	var raw strings.Builder
	raw.Grow(size)
	for _, l := range lines {
		raw.WriteString(l)
		raw.WriteByte('\r')
	}
	m.Text = raw.String()
	return m, nil
}

func get(f []string, n int) string {
	if n < len(f) {
		return f[n]
	}
	return ""
}

// checkHeader: MSH-9 (type and event), MSH-10 (control id), MSH-11
// (processing id) and MSH-12 (a 2.x version) must be readable.
func checkHeader(m *Message) *Error {
	h := m.MSH()
	typ, ev := m.Type()
	if typ == "" || ev == "" || len(typ) > 3 || len(ev) > 3 {
		return &Error{Reason: ReasonHeader}
	}
	id, err := m.Value(h, 10, 0, 1, 1)
	if err != nil || id == "" || len(id) > 199 {
		return &Error{Reason: ReasonHeader}
	}
	if v, err := m.Value(h, 12, 0, 1, 1); err != nil || !strings.HasPrefix(v, "2.") || len(v) > 10 {
		return &Error{Reason: ReasonHeader}
	}
	if p, err := m.Value(h, 11, 0, 1, 1); err != nil || p == "" {
		return &Error{Reason: ReasonHeader}
	}
	return nil
}

// Type is MSH-9.1 and MSH-9.2 (ORU, R01), as written.
func (m *Message) Type() (string, string) {
	h := m.MSH()
	t, err1 := m.Value(h, 9, 0, 1, 1)
	e, err2 := m.Value(h, 9, 0, 2, 1)
	if err1 != nil || err2 != nil {
		return "", ""
	}
	return t, e
}

// ControlID is MSH-10.
func (m *Message) ControlID() string {
	v, _ := m.Value(m.MSH(), 10, 0, 1, 1)
	return v
}

// ParseMessage parses exactly one message (the bridge's MLLP frame): no
// envelope segment, one MSH first.
func ParseMessage(data []byte) (*Message, error) {
	if len(data) > MaxMessage {
		return nil, &Error{Reason: ReasonMessageTooLarge, Message: 1}
	}
	b, err := parse(data, true)
	if err != nil {
		return nil, err
	}
	if len(b.Messages) != 1 || b.Enveloped {
		return nil, &Error{Reason: ReasonShape, Message: 1}
	}
	return b.Messages[0], nil
}

// ParseBatch parses a file: FHS/BHS … BTS/FTS envelopes, or bare messages
// one after another. Any error refuses the file whole.
func ParseBatch(data []byte) (*Batch, error) {
	b, err := parse(data, false)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func parse(data []byte, single bool) (*Batch, *Error) {
	if len(data) > MaxFile {
		return nil, &Error{Reason: ReasonTooLarge}
	}
	s := string(data)
	if strings.IndexByte(s, 0) >= 0 || strings.HasPrefix(s, "\xef\xbb\xbf") {
		return nil, &Error{Reason: ReasonEncoding}
	}
	b := &Batch{}
	var (
		fileD, batchD          Delims
		inFile, inBatch, ended bool
		batches, inThisBatch   int
		cur                    []string
		ferr                   *Error
		segs                   int
	)
	flush := func() bool {
		if cur == nil {
			return true
		}
		if len(b.Messages) >= MaxMessages {
			ferr = &Error{Reason: ReasonTooMany}
			return false
		}
		m, err := buildMessage(cur, len(b.Messages)+1)
		if err != nil {
			ferr = err
			return false
		}
		b.Messages = append(b.Messages, m)
		inThisBatch++
		cur = nil
		return true
	}
	envelope := func(line string, d Delims) ([]string, bool) {
		if !bytesOK(line, CharsetUTF8) {
			ferr = &Error{Reason: ReasonEncoding}
			return nil, false
		}
		f, ok := splitFields(line, d, line[:3] == "FHS" || line[:3] == "BHS")
		if !ok {
			ferr = &Error{Reason: ReasonEnvelope}
		}
		return f, ok
	}
	count := func(raw string, want int) bool {
		v, _, _ := strings.Cut(raw, "^")
		if v == "" {
			return true
		}
		if len(v) > 7 {
			return false
		}
		n := 0
		for i := 0; i < len(v); i++ {
			if v[i] < '0' || v[i] > '9' {
				return false
			}
			n = n*10 + int(v[i]-'0')
		}
		return n == want
	}
	reason, ok := splitLines(s, func(line string) bool {
		segs++
		if ended {
			ferr = &Error{Reason: ReasonEnvelope} // after FTS
			return false
		}
		if len(line) < 3 || !isSegID(line[:3]) {
			ferr = &Error{Reason: ReasonShape, Message: len(b.Messages) + 1}
			return false
		}
		id := line[:3]
		switch id {
		case "FHS", "BHS", "BTS", "FTS":
			if single {
				ferr = &Error{Reason: ReasonShape, Message: 1}
				return false
			}
			if !flush() {
				return false
			}
			b.Enveloped = true
		}
		switch id {
		case "FHS":
			if segs != 1 {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			d, ok := delimsOf(line)
			if !ok {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			if _, ok := envelope(line, d); !ok {
				return false
			}
			fileD, inFile = d, true
		case "BHS":
			if inBatch {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			if !inFile && (segs != 1 || batches > 0) {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			d, ok := delimsOf(line)
			if !ok {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			if _, ok := envelope(line, d); !ok {
				return false
			}
			batchD, inBatch, inThisBatch = d, true, 0
			if !inFile && len(b.Messages) > 0 {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
		case "BTS":
			if !inBatch || (len(line) > 3 && line[3] != batchD.Field) {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			f, ok := envelope(line, batchD)
			if !ok {
				return false
			}
			if !count(strings.ReplaceAll(get(f, 1), string(batchD.Comp), "^"), inThisBatch) {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			inBatch = false
			batches++
			if !inFile {
				ended = true
			}
		case "FTS":
			if !inFile || inBatch || (len(line) > 3 && line[3] != fileD.Field) {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			f, ok := envelope(line, fileD)
			if !ok {
				return false
			}
			if !count(strings.ReplaceAll(get(f, 1), string(fileD.Comp), "^"), batches) {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			ended = true
		case "MSH":
			if !flush() {
				return false
			}
			if inFile && !inBatch {
				ferr = &Error{Reason: ReasonEnvelope} // a message outside any batch of the file
				return false
			}
			if b.Enveloped && !inBatch {
				ferr = &Error{Reason: ReasonEnvelope}
				return false
			}
			cur = []string{line}
		default:
			if cur == nil {
				ferr = &Error{Reason: ReasonShape, Message: len(b.Messages) + 1}
				return false
			}
			if len(cur) >= MaxSegmentsPerMsg {
				ferr = &Error{Reason: ReasonShape, Message: len(b.Messages) + 1}
				return false
			}
			cur = append(cur, line)
		}
		return true
	})
	if !ok {
		return nil, &Error{Reason: reason}
	}
	if ferr != nil {
		return nil, ferr
	}
	if !flush() {
		return nil, ferr
	}
	if (inFile && !ended) || inBatch {
		return nil, &Error{Reason: ReasonEnvelope} // cut short
	}
	if len(b.Messages) == 0 {
		return nil, &Error{Reason: ReasonShape}
	}
	return b, nil
}

// Complete reports whether a file looks finished: it ends with a segment
// terminator, and one that opens with FHS (or BHS) ends with FTS (or BTS).
// A file that isn't may still be being written; the connector leaves it.
func Complete(data []byte) bool {
	n := len(data)
	if n == 0 || (data[n-1] != '\r' && data[n-1] != '\n') {
		return false
	}
	trimmed := strings.TrimRight(string(data), "\r\n")
	last := trimmed
	if i := strings.LastIndexAny(trimmed, "\r\n"); i >= 0 {
		last = trimmed[i+1:]
	}
	switch {
	case strings.HasPrefix(string(data), "FHS"):
		return strings.HasPrefix(last, "FTS")
	case strings.HasPrefix(string(data), "BHS"):
		return strings.HasPrefix(last, "BTS")
	}
	return true
}
