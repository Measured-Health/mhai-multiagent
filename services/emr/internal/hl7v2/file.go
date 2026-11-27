package hl7v2

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Format is the file format's name and version (FHS-10 of a file the
// bridge writes; the format document).
const Format = "hl7v2_file_v1"

// fileName is a file the connector reads: hl7-<UTC time>[-<seq>].hl7.
var fileName = regexp.MustCompile(`^hl7-([0-9]{8}T[0-9]{6}Z)(?:-([0-9]{1,4}))?\.hl7$`)

// FileName is the name of the file written at t (UTC) with sequence seq
// (0 to 9999) within that second.
func FileName(t time.Time, seq int) string {
	return fmt.Sprintf("hl7-%s-%04d.hl7", t.UTC().Format("20060102T150405Z"), seq%10000)
}

// ParseFileName reports whether name is a file to read, with its time (as
// written, sortable) and sequence.
func ParseFileName(name string) (at string, seq int, ok bool) {
	m := fileName.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	if _, err := time.Parse("20060102T150405Z", m[1]); err != nil {
		return "", 0, false
	}
	if m[2] != "" {
		seq, _ = strconv.Atoi(m[2])
	}
	return m[1], seq, true
}

// dtm is HL7's DTM: YYYY[MM[DD[HH[MM[SS[.S{1,4}]]]]]][+/-ZZZZ].
var dtm = regexp.MustCompile(`^([0-9]{4})([0-9]{2})([0-9]{2})(?:([0-9]{2})([0-9]{2})(?:([0-9]{2})(?:\.([0-9]{1,4}))?)?)?([+-][0-9]{4})?$`)

// ErrTime: a time that can't be read without guessing.
var ErrTime = errors.New("hl7v2: time unreadable")

// ParseTime reads an HL7 DTM (design §3.5): with minutes or more it is a
// date-time, read with its offset, or in loc when it has none (refused
// when loc is nil); a date alone is a date ("2006-01-02", dateOnly). It
// returns RFC 3339 (or the date), which the normaliser reads as it reads
// FHIR. "" is "" and no error.
func ParseTime(s string, loc *time.Location) (out string, dateOnly bool, err error) {
	if s == "" {
		return "", false, nil
	}
	p := dtm.FindStringSubmatch(s)
	if p == nil {
		return "", false, ErrTime
	}
	atoi := func(v string) int { n, _ := strconv.Atoi(v); return n }
	y, mo, d := atoi(p[1]), atoi(p[2]), atoi(p[3])
	if p[4] == "" {
		if p[8] != "" {
			return "", false, ErrTime // a date with an offset: not a form we read
		}
		t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
		if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
			return "", false, ErrTime
		}
		return t.Format("2006-01-02"), true, nil
	}
	h, mi, sec := atoi(p[4]), atoi(p[5]), atoi(p[6])
	ns := 0
	if p[7] != "" {
		frac := p[7] + strings.Repeat("0", 9-len(p[7]))
		ns = atoi(frac)
	}
	if h > 23 || mi > 59 || sec > 59 {
		return "", false, ErrTime
	}
	zone := loc
	if p[8] != "" {
		off := atoi(p[8][1:3])*3600 + atoi(p[8][3:5])*60
		if atoi(p[8][1:3]) > 14 || atoi(p[8][3:5]) > 59 {
			return "", false, ErrTime
		}
		if p[8][0] == '-' {
			off = -off
		}
		zone = time.FixedZone("", off)
	}
	if zone == nil {
		return "", false, ErrTime
	}
	t := time.Date(y, time.Month(mo), d, h, mi, sec, ns, zone)
	if t.Year() != y || int(t.Month()) != mo || t.Day() != d {
		return "", false, ErrTime
	}
	return t.Format(time.RFC3339Nano), false, nil
}

// FormatTime is t as an HL7 DTM with its offset, to the second.
func FormatTime(t time.Time) string { return t.Format("20060102150405-0700") }

// FileHeader is what the bridge writes at the start of a file: FHS and BHS
// with the standard delimiters, naming the file and the format.
func FileHeader(name string, at time.Time) string {
	ts := FormatTime(at)
	return "FHS|^~\\&|MHAI-HL7-BRIDGE||||" + ts + "||" + name + "|" + Format + "\r" +
		"BHS|^~\\&|MHAI-HL7-BRIDGE||||" + ts + "\r"
}

// FileTrailer closes a file holding n messages in one batch.
func FileTrailer(n int) string {
	return "BTS|" + strconv.Itoa(n) + "\rFTS|1\r"
}
