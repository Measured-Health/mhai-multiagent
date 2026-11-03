package feedback

import (
	"encoding/json"
	"testing"
	"time"
)

// R11: reason and the rejection note may span lines (\n, \r\n); every
// other control character, and every Unicode format character (bidi
// overrides, zero-width), is refused; the corrected diagnosis stays one
// line.
func TestTextLineBreaks(t *testing.T) {
	str := func(s string) *string { return &s }
	now := time.Now()
	correct := func(diagnosis, reason string) bool {
		_, ok := FeedbackIn{Action: "correct", CorrectedDiagnosis: str(diagnosis), Reason: str(reason)}.validate(now)
		return ok
	}
	reject := func(note string) bool {
		_, _, ok := parseReject([]byte(`{"reason_code":"unsafe","note":` + jsonString(note) + `}`))
		return ok
	}
	for name, c := range map[string]struct {
		ok  bool
		got bool
	}{
		"reason with \\n":               {true, correct("Synthetic flutter", "Line one\nline two")},
		"reason with \\r\\n":            {true, correct("Synthetic flutter", "Line one\r\nline two")},
		"reason with a lone \\r":        {false, correct("Synthetic flutter", "Line one\rline two")},
		"reason with a tab":             {false, correct("Synthetic flutter", "Line one\tline two")},
		"reason with BEL":               {false, correct("Synthetic flutter", "Line one\x07")},
		"reason with a bidi override":   {false, correct("Synthetic flutter", "Line \u202eone")},
		"reason with a zero-width":      {false, correct("Synthetic flutter", "Line\u200bone")},
		"reason with a BOM":             {false, correct("Synthetic flutter", "Line\ufeffone")},
		"diagnosis with \\n":            {false, correct("Synthetic\nflutter", "Synthetic reason")},
		"diagnosis with \\r\\n":         {false, correct("Synthetic\r\nflutter", "Synthetic reason")},
		"diagnosis with a bidi isolate": {false, correct("Synthetic \u2066flutter", "Synthetic reason")},
		"diagnosis with U+2028":         {false, correct("Synthetic\u2028flutter", "Synthetic reason")},
		"diagnosis with U+2029":         {false, correct("Synthetic\u2029flutter", "Synthetic reason")},
		"reason with U+2028":            {false, correct("Synthetic flutter", "Line one\u2028line two")},
		"note with U+2029":              {false, reject("Note one\u2029note two")},
		"plain":                         {true, correct("Synthetic flutter", "Synthetic reason")},
		"note with \\n":                 {true, reject("Note one\nnote two")},
		"note with \\r\\n":              {true, reject("Note one\r\nnote two")},
		"note with a lone \\r":          {false, reject("Note one\rnote two")},
		"note with NUL":                 {false, reject("Note\x00")},
		"note with a zero-width joiner": {false, reject("Note\u200dtwo")},
		"note with an RTL mark":         {false, reject("Note\u200ftwo")},
	} {
		if c.got != c.ok {
			t.Errorf("%s: accepted %v, want %v", name, c.got, c.ok)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
