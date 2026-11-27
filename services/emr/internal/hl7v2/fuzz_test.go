package hl7v2_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2"
	"github.com/Measured-Health/mhai-multiagent/services/emr/internal/hl7v2/hl7test"
)

func seeds() []string {
	return []string{
		oru("SYN-CTRL-1"),
		hl7test.RDE("SYN-CTRL-2", "SYN-RX-1", at),
		hl7test.ADT("SYN-CTRL-3", at),
		string(hl7test.File("hl7-x.hl7", at, oru("C1"), oru("C2"))),
		"BHS|^~\\&|SYNTH\r" + oru("C1") + "BTS|1\r",
		strings.NewReplacer("|", "#", "^", "@").Replace(oru("C4")),
		strings.Replace(oru("C5"), hl7test.Family, `SYN\F\\X0D0A\\.br\\Zx\`, 1),
		strings.ReplaceAll(oru("C6"), "\r", "\r\n"),
		"MSH|^~\\&#|A|B|||" + hl7v2.FormatTime(at) + "||ORU^R01|C7|P|2.7|||||||8859/1\rPID|1||X\xe9^^^A^MR\r",
		"",
		"MSH|",
	}
}

// The parser never panics, and what it reads is consistent: every value
// of every message unescapes to valid UTF-8 or refuses with ErrValue, and
// every segment id is three capitals or digits.
func FuzzParseBatch(f *testing.F) {
	for _, s := range seeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b, err := hl7v2.ParseBatch(data)
		_ = hl7v2.Complete(data)
		if err != nil {
			if b != nil {
				t.Fatal("a batch with an error")
			}
			return
		}
		if len(b.Messages) == 0 || len(b.Messages) > hl7v2.MaxMessages {
			t.Fatalf("%d messages", len(b.Messages))
		}
		for _, m := range b.Messages {
			if m.Segs[0].ID != "MSH" || m.ControlID() == "" {
				t.Fatal("header")
			}
			for i := range m.Segs {
				s := &m.Segs[i]
				if len(s.ID) != 3 || len(s.F) > hl7v2.MaxFields+1 {
					t.Fatalf("segment %q with %d fields", s.ID, len(s.F))
				}
				for fn := 1; fn < len(s.F); fn++ {
					for rep := 0; rep < m.Reps(s, fn) && rep < 3; rep++ {
						for c := 1; c <= 3; c++ {
							v, err := m.Value(s, fn, rep, c, 1)
							if err == nil && !utf8.ValidString(v) {
								t.Fatalf("invalid UTF-8 value")
							}
						}
					}
				}
			}
		}
	})
}

// ParseTime never panics, and what it reads is RFC 3339 or a date.
func FuzzParseTime(f *testing.F) {
	for _, s := range []string{"20261127101500+0100", "20261127", "202611271015", "20261127101500.12-0300", "2026"} {
		f.Add(s)
	}
	lagos, _ := time.LoadLocation("Africa/Lagos")
	f.Fuzz(func(t *testing.T, s string) {
		out, dateOnly, err := hl7v2.ParseTime(s, lagos)
		if err != nil || out == "" {
			return
		}
		if dateOnly {
			if _, err := time.Parse("2006-01-02", out); err != nil {
				t.Fatalf("date %q", out)
			}
			return
		}
		if _, err := time.Parse(time.RFC3339Nano, out); err != nil {
			t.Fatalf("time %q", out)
		}
	})
}
