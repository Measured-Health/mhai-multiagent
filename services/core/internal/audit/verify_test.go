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
	key, err := RunCursorKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := RunCursorKey([]byte(strings.Repeat("j", 32)))
	cs := RunCursors{Key: key, Session: "s1"}
	c := RunCursor{StartedAt: time.Date(2026, 11, 3, 6, 0, 0, 123456000, time.UTC), ID: uuid.New()}
	s := cs.Encode(c)
	if len(s) != 80 || strings.ToLower(s) != s {
		t.Fatalf("cursor %q", s)
	}
	back, err := cs.Decode(s)
	if err != nil || !back.StartedAt.Equal(c.StartedAt) || back.ID != c.ID {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	madeWithKey := RunCursors{Key: key, Session: "s1"}.Encode(RunCursor{StartedAt: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), ID: uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")})
	// A hand-made cursor (year 3000, all-f id, all-f tag) has no valid tag.
	handMade := "00e0" + strings.Repeat("0", 4) + strings.Repeat("f", 72)
	for _, bad := range []string{"", s[:79], s + "0", strings.ToUpper(s), "zz" + s[2:], strings.Repeat("0", 80), strings.Repeat("f", 80), handMade} {
		if _, err := cs.Decode(bad); !errors.Is(err, ErrCursor) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Another session's, another key's: refused. (madeWithKey is valid for s1.)
	if _, err := (RunCursors{Key: key, Session: "s2"}).Decode(s); !errors.Is(err, ErrCursor) {
		t.Errorf("another session: %v", err)
	}
	if _, err := (RunCursors{Key: other, Session: "s1"}).Decode(s); !errors.Is(err, ErrCursor) {
		t.Errorf("another key: %v", err)
	}
	if _, err := (RunCursors{Key: key, Session: "s1"}).Decode(madeWithKey); err != nil {
		t.Errorf("a cursor made with the key: %v", err)
	}
}
