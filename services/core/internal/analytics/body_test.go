package analytics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

func okBody(p calendar.Period) Body {
	from, to := Dates(p)
	row := Row{Questions: smallgroup.Count(12), Answers: smallgroup.Count(12), Pass: smallgroup.Count(12), Regenerate: smallgroup.Count(0),
		Escalate: smallgroup.Count(0), Degraded: smallgroup.Count(0), Reviews: smallgroup.Count(0), ActiveClinicians: smallgroup.Count(6),
		QuestionsPerClinician: RatioOf(2)}
	return Body{Kind: string(p.Kind), From: from, To: to, All: row,
		Hospitals: []HospitalRow{{Hospital: Ref{ID: uuid.NewString(), Name: "Synthetic General"}, Staff: StaffFigure(9), Row: row}}}
}

// A16: a stored body with a shown 3, an unknown key, another period, a bad
// id or name is refused; a good one round-trips byte for byte.
func TestBodyDecodeStrict(t *testing.T) {
	p := calendar.Of(calendar.Week, time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC))
	good, _ := json.Marshal(okBody(p))
	back, err := DecodeBody(good, p)
	if err != nil {
		t.Fatalf("a good body refused: %v (%s)", err, good)
	}
	again, _ := json.Marshal(back)
	if string(again) != string(good) {
		t.Errorf("round trip\n%s\n%s", good, again)
	}
	for name, b := range map[string]string{
		"a shown 3":       strings.Replace(string(good), `"questions":{"value":12,"suppressed":false}`, `"questions":{"value":3,"suppressed":false}`, 1),
		"an unknown key":  strings.Replace(string(good), `"kind":`, `"raw":{"q_pass":3},"kind":`, 1),
		"hidden w/ value": strings.Replace(string(good), `"pass":{"value":12,"suppressed":false}`, `"pass":{"value":12,"suppressed":true}`, 1),
		"a bad id":        strings.Replace(string(good), `"id":"`, `"id":"X`, 1),
		"a bad name":      strings.Replace(string(good), `Synthetic General`, `Synthetic\u0000General`, 1),
		"a ratio of -1":   strings.Replace(string(good), `"questions_per_clinician":{"value":2.00,"suppressed":false}`, `"questions_per_clinician":{"value":-1,"suppressed":false}`, 1),
		"empty":           ``,
		"trailing":        string(good) + `{}`,
	} {
		if b == string(good) {
			t.Fatalf("%s: the replacement didn't apply", name)
		}
		if _, err := DecodeBody([]byte(b), p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := DecodeBody(good, calendar.Next(p)); err == nil {
		t.Error("another period accepted")
	}
	if _, err := DecodeBody(good, calendar.Of(calendar.Month, p.From)); err == nil {
		t.Error("another kind accepted")
	}
	// A hospital whose name can't be shown: an empty name, no staff, every
	// figure hidden.
	b := okBody(p)
	b.Hospitals[0].Hospital.Name, b.Hospitals[0].Staff = "", smallgroup.Guarded{Suppressed: true}
	raw, _ := json.Marshal(b)
	if _, err := DecodeBody(raw, p); err == nil {
		t.Error("an unnamed row with figures accepted")
	}
	hid := smallgroup.Guarded{Suppressed: true}
	b.Hospitals[0].Row = Row{Questions: hid, Answers: hid, Pass: hid, Regenerate: hid, Escalate: hid, Degraded: hid, Reviews: hid,
		ActiveClinicians: hid, QuestionsPerClinician: Ratio{Number{Suppressed: true}}}
	raw, _ = json.Marshal(b)
	if _, err := DecodeBody(raw, p); err != nil {
		t.Errorf("an unnamed, fully hidden row refused: %v", err)
	}
}

// A ratio or a median is never written as a bare 1 to 4.
func TestNumbersKeepTheirDecimals(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{RatioOf(4), `{"value":4.00,"suppressed":false}`},
		{RatioOf(2.456), `{"value":2.46,"suppressed":false}`},
		{HoursOf(3), `{"value":3.0,"suppressed":false}`},
		{Ratio{Number{Suppressed: true}}, `{"value":null,"suppressed":true}`},
		{Hours{}, `{"value":null,"suppressed":false}`},
	} {
		b, _ := json.Marshal(c.v)
		if string(b) != c.want {
			t.Errorf("%+v encodes %s, want %s", c.v, b, c.want)
		}
	}
}
