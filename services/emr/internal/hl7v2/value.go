package hl7v2

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// ErrValue: a value can't be read as given (an unknown, unterminated or
// character-set-switching escape, or decoded bytes invalid in the
// message's character set). A fixed value: the mapper keeps the record
// only as a dead letter.
var ErrValue = errors.New("hl7v2: value unreadable")

// nth is the n-th (0-based) part of s split on sep, or "".
func nth(s string, sep byte, n int) string {
	for i := 0; i < n; i++ {
		j := strings.IndexByte(s, sep)
		if j < 0 {
			return ""
		}
		s = s[j+1:]
	}
	if j := strings.IndexByte(s, sep); j >= 0 {
		return s[:j]
	}
	return s
}

// Raw is field f, repetition rep (0-based), component c and subcomponent
// sub (1-based) of seg, still escaped. MSH-1 and MSH-2 are returned whole.
func (m *Message) Raw(seg *Segment, f, rep, c, sub int) string {
	v := seg.Field(f)
	if (seg.ID == "MSH" || seg.ID == "FHS" || seg.ID == "BHS") && f <= 2 {
		return v
	}
	v = nth(v, m.D.Rep, rep)
	v = nth(v, m.D.Comp, c-1)
	return nth(v, m.D.Sub, sub-1)
}

// Reps is how many repetitions field f of seg has (0 when empty).
func (m *Message) Reps(seg *Segment, f int) int {
	v := seg.Field(f)
	if v == "" {
		return 0
	}
	return strings.Count(v, string(m.D.Rep)) + 1
}

// Value is the unescaped value at the position (see Raw), with leading and
// trailing spaces removed. HL7's explicit null ("") reads as empty.
func (m *Message) Value(seg *Segment, f, rep, c, sub int) (string, error) {
	raw := m.Raw(seg, f, rep, c, sub)
	if (seg.ID == "MSH" || seg.ID == "FHS" || seg.ID == "BHS") && f <= 2 {
		return raw, nil // the delimiters themselves, never escaped
	}
	if raw == `""` {
		return "", nil
	}
	v, err := m.Unescape(raw)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

// V is Value of the first repetition and subcomponent; an unreadable value
// reads as ok=false.
func (m *Message) V(seg *Segment, f, c int) (string, bool) {
	v, err := m.Value(seg, f, 0, c, 1)
	return v, err == nil
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Unescape decodes HL7 escape sequences in s (design §3.3). The result is
// never longer than s, plus one space per formatting command.
func (m *Message) Unescape(s string) (string, error) {
	e := m.D.Esc
	if strings.IndexByte(s, e) < 0 {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != e {
			b.WriteByte(s[i])
			continue
		}
		j := strings.IndexByte(s[i+1:], e)
		if j < 0 || j > MaxEscape {
			return "", ErrValue
		}
		seq := s[i+1 : i+1+j]
		i += j + 1
		switch {
		case seq == "F":
			b.WriteByte(m.D.Field)
		case seq == "S":
			b.WriteByte(m.D.Comp)
		case seq == "T":
			b.WriteByte(m.D.Sub)
		case seq == "R":
			b.WriteByte(m.D.Rep)
		case seq == "E":
			b.WriteByte(m.D.Esc)
		case seq == "P" && m.D.Trunc != 0:
			b.WriteByte(m.D.Trunc)
		case seq == "H", seq == "N":
			// highlighting on and off: dropped
		case strings.HasPrefix(seq, ".") && len(seq) <= 8 && formatting(seq[1:]):
			b.WriteByte(' ')
		case strings.HasPrefix(seq, "X") && len(seq) > 1 && len(seq)%2 == 1:
			raw := make([]byte, 0, len(seq)/2)
			for k := 1; k < len(seq); k += 2 {
				hi, ok1 := hexVal(seq[k])
				lo, ok2 := hexVal(seq[k+1])
				if !ok1 || !ok2 {
					return "", ErrValue
				}
				raw = append(raw, hi<<4|lo)
			}
			out, ok := m.decodeBytes(raw)
			if !ok {
				return "", ErrValue
			}
			b.WriteString(out)
		default:
			// \C..\ and \M..\ switch character sets, \Z..\ is locally
			// defined: never guessed.
			return "", ErrValue
		}
	}
	return b.String(), nil
}

// formatting: the FT formatting commands (\.br\, \.sp<n>\, ...).
func formatting(cmd string) bool {
	for _, p := range []string{"br", "sp", "fi", "nf", "in", "ti", "sk", "ce"} {
		if strings.HasPrefix(cmd, p) {
			rest := strings.TrimPrefix(strings.TrimPrefix(cmd[len(p):], "+"), "-")
			for i := 0; i < len(rest); i++ {
				if rest[i] < '0' || rest[i] > '9' {
					return false
				}
			}
			return true
		}
	}
	return false
}

// decodeBytes reads \X..\ bytes in the message's character set; a control
// character other than CR, LF or tab is refused (it is text, never a
// delimiter: the value was split already).
func (m *Message) decodeBytes(raw []byte) (string, bool) {
	var s string
	switch m.Charset {
	case CharsetASCII:
		for _, c := range raw {
			if c >= 0x80 {
				return "", false
			}
		}
		s = string(raw)
	case CharsetUTF8:
		if !utf8.Valid(raw) {
			return "", false
		}
		s = string(raw)
	case CharsetLatin1:
		s = latin1ToUTF8(string(raw))
	default:
		return "", false
	}
	for _, r := range s {
		if r == 0 || (r < 0x20 && r != '\r' && r != '\n' && r != '\t') || r == 0x7f {
			return "", false
		}
	}
	return s, true
}

// Escape escapes s for writing in a value with d (the bridge's ACK echoes
// ids it read).
func Escape(s string, d Delims) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case d.Esc:
			b.WriteString(string(d.Esc) + "E" + string(d.Esc))
		case d.Field:
			b.WriteString(string(d.Esc) + "F" + string(d.Esc))
		case d.Comp:
			b.WriteString(string(d.Esc) + "S" + string(d.Esc))
		case d.Sub:
			b.WriteString(string(d.Esc) + "T" + string(d.Esc))
		case d.Rep:
			b.WriteString(string(d.Esc) + "R" + string(d.Esc))
		case '\r', '\n':
			b.WriteByte(' ')
		default:
			if d.Trunc != 0 && c == d.Trunc {
				b.WriteString(string(d.Esc) + "P" + string(d.Esc))
				continue
			}
			b.WriteByte(c)
		}
	}
	return b.String()
}
