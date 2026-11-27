package hl7v2_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
)

var at = time.Date(2026, 11, 27, 10, 15, 0, 0, time.UTC)

func reason(t *testing.T, err error) string {
	t.Helper()
	var e *hl7v2.Error
	if !errors.As(err, &e) {
		t.Fatalf("not a refusal: %v", err)
	}
	return e.Reason
}

func oru(ctrl string) string { return hl7test.ORU(ctrl, "SYN-ORD-1", at, hl7test.Potassium(at)) }

func TestParseMessageFields(t *testing.T) {
	m, err := hl7v2.ParseMessage([]byte(oru("SYN-CTRL-1")))
	if err != nil {
		t.Fatal(err)
	}
	if typ, ev := m.Type(); typ != "ORU" || ev != "R01" || m.ControlID() != "SYN-CTRL-1" || m.Charset != hl7v2.CharsetUTF8 {
		t.Fatalf("header %s %s %s %s", typ, ev, m.ControlID(), m.Charset)
	}
	var pid *hl7v2.Segment
	for i := range m.Segs {
		if m.Segs[i].ID == "PID" {
			pid = &m.Segs[i]
		}
	}
	if pid == nil || m.Reps(pid, 3) != 2 {
		t.Fatal("PID-3 repetitions")
	}
	if v, _ := m.Value(pid, 3, 1, 5, 1); v != "NI" {
		t.Fatalf("PID-3[1].5 = %q", v)
	}
	if v, _ := m.V(pid, 5, 1); v != hl7test.Family {
		t.Fatalf("family %q", v)
	}
	if v, _ := m.V(m.MSH(), 2, 1); v != `^~\&` {
		t.Fatalf("MSH-2 %q", v)
	}
}

// Segment terminators: \r, \n and \r\n all read alike; empty lines are
// skipped.
func TestTerminators(t *testing.T) {
	base := oru("SYN-CTRL-1")
	for name, s := range map[string]string{"cr": base, "lf": strings.ReplaceAll(base, "\r", "\n"),
		"crlf": strings.ReplaceAll(base, "\r", "\r\n"), "blank lines": strings.ReplaceAll(base, "\r", "\r\n\n")} {
		m, err := hl7v2.ParseMessage([]byte(s))
		if err != nil || len(m.Segs) != 6 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Delimiters come from each message: another set reads the same; a bad
// set refuses the message.
func TestDelimitersFromTheMessage(t *testing.T) {
	alt := strings.NewReplacer("|", "#", "^", "@", "~", "*", `\`, "!", "&", "$").Replace(oru("SYN-CTRL-1"))
	m, err := hl7v2.ParseMessage([]byte(alt))
	if err != nil {
		t.Fatal(err)
	}
	if m.D.Field != '#' || m.ControlID() != "SYN-CTRL-1" {
		t.Fatalf("delims %+v", m.D)
	}
	for name, enc := range map[string]string{"repeated": "^^\\&", "letter": "^~\\a", "short": "^~\\", "digit": "^~1&",
		"field sep reused": "^|\\&", "space": "^ \\&"} {
		s := strings.Replace(oru("SYN-CTRL-1"), `MSH|^~\&|`, "MSH|"+enc+"|", 1)
		if _, err := hl7v2.ParseMessage([]byte(s)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// Escapes decode after splitting: an escaped delimiter or CR is text,
// never a split.
func TestEscapesNeverSplit(t *testing.T) {
	s := strings.Replace(oru("SYN-CTRL-1"), hl7test.Family, `SYN\F\FAM\S\X\R\Y\T\Z\E\\X0D\\H\END\N\`, 1)
	m, err := hl7v2.ParseMessage([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	pid := &m.Segs[1]
	if m.Reps(pid, 5) != 1 || len(pid.F) != 9 {
		t.Fatalf("split: %d reps, %d fields", m.Reps(pid, 5), len(pid.F))
	}
	v, err := m.Value(pid, 5, 0, 1, 1)
	if err != nil || v != "SYN|FAM^X~Y&Z\\\rEND" {
		t.Fatalf("value %q %v", v, err)
	}
	for _, bad := range []string{`\C2842\`, `\M2442\`, `\Zlocal\`, `\X0\`, `\XZZ\`, `\X00\`, `unterminated\F`, `\X1B\`} {
		s := strings.Replace(oru("SYN-CTRL-1"), hl7test.Family, bad, 1)
		m, err := hl7v2.ParseMessage([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Value(&m.Segs[1], 5, 0, 1, 1); !errors.Is(err, hl7v2.ErrValue) {
			t.Errorf("%q read", bad)
		}
	}
	if m, _ := hl7v2.ParseMessage([]byte(strings.Replace(oru("SYN-CTRL-1"), hl7test.Family, `A\.br\B\.sp2\C`, 1))); m != nil {
		if v, _ := m.Value(&m.Segs[1], 5, 0, 1, 1); v != "A B C" {
			t.Errorf("formatting %q", v)
		}
	}
}

// Character sets: only those declared and readable; never guessed.
func TestCharsets(t *testing.T) {
	withCS := func(cs, family string) []byte {
		s := strings.Replace(oru("SYN-CTRL-1"), "|UNICODE UTF-8", "|"+cs, 1)
		return []byte(strings.Replace(s, hl7test.Family, family, 1))
	}
	if m, err := hl7v2.ParseMessage(withCS("8859/1", "SYN\xe9")); err != nil {
		t.Fatal(err)
	} else if v, _ := m.V(&m.Segs[1], 5, 1); v != "SYNé" {
		t.Fatalf("latin1 %q", v)
	}
	if _, err := hl7v2.ParseMessage(withCS("UNICODE UTF-8", "SYNé")); err != nil {
		t.Fatal(err)
	}
	if _, err := hl7v2.ParseMessage(withCS("", "SYNTH")); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"utf-16":            withCS("UNICODE UTF-16", "SYNTH"),
		"8859/2":            withCS("8859/2", "SYNTH"),
		"iso ir87":          withCS("ISO IR87", "SYNTH"),
		"two sets":          withCS("ASCII~ISO IR87", "SYNTH"),
		"bad utf-8":         withCS("UNICODE UTF-8", "SYN\xc3\x28"),
		"overlong utf-8":    withCS("UNICODE UTF-8", "SYN\xc0\xaf"),
		"non-ascii, no set": withCS("", "SYN\xe9"),
		"nul":               withCS("", "SYN\x00"),
		"control":           withCS("", "SYN\x0b"),
		"bom":               append([]byte("\xef\xbb\xbf"), oru("SYN-CTRL-1")...),
	} {
		if _, err := hl7v2.ParseMessage(b); err == nil || reason(t, err) != hl7v2.ReasonEncoding {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestHeaderRequired(t *testing.T) {
	for name, s := range map[string]string{
		"no type":       strings.Replace(oru("C1"), "ORU^R01^ORU_R01", "", 1),
		"no event":      strings.Replace(oru("C1"), "ORU^R01^ORU_R01", "ORU", 1),
		"no control id": strings.Replace(oru("C1"), "|C1|", "||", 1),
		"version 3":     strings.Replace(oru("C1"), "|2.5.1|", "|3.0|", 1),
		"no processing": strings.Replace(oru("C1"), "|P|2.5.1|", "||2.5.1|", 1),
	} {
		if _, err := hl7v2.ParseMessage([]byte(s)); err == nil || reason(t, err) != hl7v2.ReasonHeader {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := hl7v2.ParseMessage([]byte("PID|1\rMSH|^~\\&|\r")); err == nil {
		t.Error("segment before MSH read")
	}
	if _, err := hl7v2.ParseMessage([]byte(strings.Replace(oru("C1"), "PID|", "pid|", 1))); err == nil {
		t.Error("lower-case segment id read")
	}
}

func TestBatches(t *testing.T) {
	two := oru("C1") + hl7test.RDE("C2", "SYN-RX-1", at)
	b, err := hl7v2.ParseBatch([]byte(two))
	if err != nil || len(b.Messages) != 2 || b.Enveloped {
		t.Fatalf("bare: %v", err)
	}
	f := hl7test.File("hl7-x.hl7", at, oru("C1"), oru("C2"), oru("C3"))
	if b, err = hl7v2.ParseBatch(f); err != nil || len(b.Messages) != 3 || !b.Enveloped || !hl7v2.Complete(f) {
		t.Fatalf("enveloped: %v", err)
	}
	// Batch only (BHS ... BTS), no file header.
	bo := "BHS|^~\\&|SYNTH\r" + oru("C1") + "BTS|1\r"
	if _, err := hl7v2.ParseBatch([]byte(bo)); err != nil || !hl7v2.Complete([]byte(bo)) {
		t.Fatalf("batch only: %v", err)
	}
	for name, s := range map[string]string{
		"count short":   hl7v2.FileHeader("x", at) + oru("C1") + oru("C2") + hl7v2.FileTrailer(3),
		"count long":    hl7v2.FileHeader("x", at) + oru("C1") + oru("C2") + hl7v2.FileTrailer(1),
		"no FTS":        hl7v2.FileHeader("x", at) + oru("C1") + "BTS|1\r",
		"no BTS":        hl7v2.FileHeader("x", at) + oru("C1") + "FTS|1\r",
		"after FTS":     string(hl7test.File("x", at, oru("C1"))) + oru("C2"),
		"FHS not first": oru("C1") + hl7v2.FileHeader("x", at) + oru("C2") + hl7v2.FileTrailer(1),
		"two FHS":       hl7v2.FileHeader("x", at) + hl7v2.FileHeader("x", at) + oru("C1") + hl7v2.FileTrailer(1),
		"msg outside":   "FHS|^~\\&|S\r" + oru("C1") + "FTS|0\r",
		"FTS batches":   hl7v2.FileHeader("x", at) + oru("C1") + "BTS|1\rFTS|2\r",
		"BTS count NaN": hl7v2.FileHeader("x", at) + oru("C1") + "BTS|one\rFTS|1\r",
	} {
		if _, err := hl7v2.ParseBatch([]byte(s)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if hl7v2.Complete([]byte(hl7v2.FileHeader("x", at) + oru("C1"))) {
		t.Error("an FHS file without FTS reads as complete")
	}
	if hl7v2.Complete([]byte(strings.TrimSuffix(oru("C1"), "\r"))) {
		t.Error("a file without a final terminator reads as complete")
	}
	if _, err := hl7v2.ParseBatch(nil); err == nil {
		t.Error("an empty file read")
	}
}

// Caps refuse whole, before the content is used.
func TestCaps(t *testing.T) {
	big := strings.Repeat("A", hl7v2.MaxSegment+1)
	cases := map[string]struct {
		data   string
		reason string
	}{
		"segment": {strings.Replace(oru("C1"), hl7test.Family, big, 1), hl7v2.ReasonSegmentTooLong},
		"fields":  {strings.Replace(oru("C1"), "PID|1|", "PID|1|"+strings.Repeat("|", hl7v2.MaxFields), 1), hl7v2.ReasonShape},
		"repetitions": {strings.Replace(oru("C1"), hl7test.Family, strings.Repeat("~", hl7v2.MaxRepetitions), 1),
			hl7v2.ReasonShape},
		"components": {strings.Replace(oru("C1"), hl7test.Family, strings.Repeat("^", hl7v2.MaxComponents), 1),
			hl7v2.ReasonShape},
		"subcomponents": {strings.Replace(oru("C1"), hl7test.Family, strings.Repeat("&", hl7v2.MaxSubcomponents), 1),
			hl7v2.ReasonShape},
		"segments": {oru("C1") + strings.Repeat("NTE|1\r", hl7v2.MaxSegmentsPerMsg), hl7v2.ReasonShape},
	}
	for name, c := range cases {
		if _, err := hl7v2.ParseBatch([]byte(c.data)); err == nil || reason(t, err) != c.reason {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Just under each cap reads.
	ok := strings.Replace(oru("C1"), hl7test.Family, strings.Repeat("~", hl7v2.MaxRepetitions-1), 1)
	if _, err := hl7v2.ParseBatch([]byte(ok)); err != nil {
		t.Errorf("at the repetition cap: %v", err)
	}
	// A message over 1 MiB, in segments each under the segment cap.
	var mb strings.Builder
	mb.WriteString(oru("C1"))
	for mb.Len() <= hl7v2.MaxMessage {
		mb.WriteString("NTE|1||" + strings.Repeat("B", 200<<10) + "\r")
	}
	if _, err := hl7v2.ParseBatch([]byte(mb.String())); err == nil || reason(t, err) != hl7v2.ReasonMessageTooLarge {
		t.Errorf("message: %v", err)
	}
	if _, err := hl7v2.ParseMessage([]byte(mb.String())); err == nil {
		t.Error("an over-cap single message read")
	}
	// Too many messages.
	tiny := hl7test.MSH(map[int]string{9: "ADT^A08", 10: "C", 11: "P", 12: "2.5"}) + "\r"
	if _, err := hl7v2.ParseBatch([]byte(strings.Repeat(tiny, hl7v2.MaxMessages+1))); err == nil ||
		reason(t, err) != hl7v2.ReasonTooMany {
		t.Errorf("messages: %v", err)
	}
	if _, err := hl7v2.ParseBatch(make([]byte, hl7v2.MaxFile+1)); err == nil || reason(t, err) != hl7v2.ReasonTooLarge {
		t.Errorf("file: %v", err)
	}
}

// A small file of delimiters only (a "zip bomb" in spirit) is refused in
// bounded time.
func TestDelimiterFlood(t *testing.T) {
	for _, c := range []string{"~", "^", "&", "|", "\\"} {
		s := strings.Replace(oru("C1"), hl7test.Family, strings.Repeat(c, 200<<10), 1)
		start := time.Now()
		_, _ = hl7v2.ParseBatch([]byte(s))
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%q took %v", c, d)
		}
	}
}

func TestParseTime(t *testing.T) {
	lagos, _ := time.LoadLocation("Africa/Lagos")
	for in, want := range map[string]string{
		"20261127101500+0100":      "2026-11-27T10:15:00+01:00",
		"202611271015-0500":        "2026-11-27T10:15:00-05:00",
		"20261127101500.1234+0000": "2026-11-27T10:15:00.1234Z",
		"20261127":                 "2026-11-27",
	} {
		if got, _, err := hl7v2.ParseTime(in, nil); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if got, _, err := hl7v2.ParseTime("20261127101500", lagos); err != nil || got != "2026-11-27T10:15:00+01:00" {
		t.Errorf("in zone: %q %v", got, err)
	}
	for _, bad := range []string{"20261127101500", "2026112710", "2026", "20261327", "20261127256000+0000",
		"20261127+0100", "2026-11-27T10:15:00Z", "20261127101500+1900", "x"} {
		if _, _, err := hl7v2.ParseTime(bad, nil); err == nil {
			t.Errorf("%q read", bad)
		}
	}
}

func TestFileNames(t *testing.T) {
	n := hl7v2.FileName(at, 7)
	if n != "hl7-20261127T101500Z-0007.hl7" {
		t.Fatal(n)
	}
	if a, seq, ok := hl7v2.ParseFileName(n); !ok || a != "20261127T101500Z" || seq != 7 {
		t.Fatal("round trip")
	}
	for _, bad := range []string{"hl7-20261127T101500Z.hl7.part", "../hl7-20261127T101500Z.hl7", "hl7-20261327T101500Z.hl7",
		"results-20261127T101500Z.csv", "HL7-20261127T101500Z.hl7", "hl7-20261127T101500Z-00001.hl7"} {
		if _, _, ok := hl7v2.ParseFileName(bad); ok {
			t.Errorf("%q read", bad)
		}
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	m, _ := hl7v2.ParseMessage([]byte(oru("C1")))
	for _, s := range []string{`a|b^c~d\e&f`, "plain", ""} {
		if got, err := m.Unescape(hl7v2.Escape(s, m.D)); err != nil || got != s {
			t.Errorf("%q: %q %v", s, got, err)
		}
	}
}
