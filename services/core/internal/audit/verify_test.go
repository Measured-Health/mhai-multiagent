package audit

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// C24: a walk is intact only with every proof; a spent budget is
// timed_out; a database error is error, never intact.
func TestOutcomeNeverIntactWithoutProof(t *testing.T) {
	at := time.Now()
	seq := int64(3)
	full := Result{EntriesChecked: 3, ToSeq: 3, ToAt: &at, Complete: true}
	cases := []struct {
		name        string
		res         Result
		err         error
		state, code string
	}{
		{"full", full, nil, "intact", ""},
		{"budget", full, ErrBudget, "error", "timed_out"},
		{"wrapped budget", full, fmt.Errorf("x: %w", ErrBudget), "error", "timed_out"},
		{"database error", full, fmt.Errorf("x: %w", &pgconn.PgError{Code: "57014"}), "error", "unexpected_reply"},
		{"connection", full, errors.New("dial"), "error", "unreachable"},
		{"not complete", Result{EntriesChecked: 3, ToSeq: 3, ToAt: &at}, nil, "error", "unexpected_reply"},
		{"no entries", Result{Complete: true}, nil, "error", "unexpected_reply"},
		{"no time", Result{EntriesChecked: 3, ToSeq: 3, Complete: true}, nil, "error", "unexpected_reply"},
		{"no seq", Result{EntriesChecked: 3, ToAt: &at, Complete: true}, nil, "error", "unexpected_reply"},
		{"broken", Result{EntriesChecked: 2, FirstBroken: &seq, FirstBrokenID: "3", FirstBrokenAt: &at}, nil, "broken", ""},
	}
	for _, c := range cases {
		if s, code := outcome(c.res, c.err); s != c.state || code != c.code {
			t.Errorf("%s: %s/%s, want %s/%s", c.name, s, code, c.state, c.code)
		}
	}
}

func TestOutcomeNoteOK(t *testing.T) {
	for _, ok := range []string{"Synthetic note.", "Line one\nline two\r\nthree", strings.Repeat("n", 1000), "é" + strings.Repeat("n", 999)} {
		if !OutcomeNoteOK(ok) {
			t.Errorf("refused %q", ok)
		}
	}
	for _, bad := range []string{"", "   \n ", strings.Repeat("n", 1001), strings.Repeat("😀", 501), "tab\there",
		"bidi\u202e", "zero\u200bwidth", "line\u2028sep", "para\u2029sep", "nul\x00", "see P-ABCD12 here", "\xff"} {
		if OutcomeNoteOK(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestRunCursorRoundTrip(t *testing.T) {
	c := RunCursor{StartedAt: time.Date(2026, 11, 3, 6, 0, 0, 123456000, time.UTC), ID: uuid.New()}
	s := c.Encode()
	if len(s) != 48 || strings.ToLower(s) != s {
		t.Fatalf("cursor %q", s)
	}
	back, err := DecodeRunCursor(s)
	if err != nil || !back.StartedAt.Equal(c.StartedAt) || back.ID != c.ID {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	for _, bad := range []string{"", s[:47], s + "0", strings.ToUpper(s), "zz" + s[2:], strings.Repeat("0", 48)} {
		if _, err := DecodeRunCursor(bad); !errors.Is(err, ErrCursor) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
