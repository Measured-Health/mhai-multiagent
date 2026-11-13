package analysis

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// One computation per context in a copy, and at most MaxFlights at once:
// past them a cold request is not ready at once.
func TestFlightsBounded(t *testing.T) {
	var e Engine
	for i := 0; i < MaxFlights; i++ {
		if _, leader, full := e.join(fmt.Sprint(i)); !leader || full {
			t.Fatalf("flight %d: leader %v, full %v", i, leader, full)
		}
	}
	if _, leader, full := e.join("0"); leader || full {
		t.Error("a second request for a context in flight doesn't follow it")
	}
	if _, _, full := e.join("new"); !full {
		t.Error("a flight past MaxFlights started")
	}
	e.leave("0")
	if _, leader, full := e.join("new"); !leader || full {
		t.Error("no flight after one left")
	}
}

// Each filter set has its own key; the filters body's is none of them.
func TestHashDistinct(t *testing.T) {
	seen := map[string]bool{string(filtersBodyHash): true}
	for _, f := range []disclose.Filters{{}, {disclose.DimSex: "female"}, {disclose.DimAgeBand: "female"},
		{disclose.DimSex: "female", disclose.DimAgeBand: "18-39"}} {
		h := string(Hash(f))
		if seen[h] {
			t.Errorf("%v: a key repeats", f)
		}
		seen[h] = true
	}
}

// The stored record reads back strictly: the same body, an unknown field
// refused, a shown count of 1 to 4 refused.
func TestRecordRoundTrip(t *testing.T) {
	p := calendar.Of(calendar.Week, mustDay("2026-10-01"))
	b := disclose.HiddenWhole(disclose.Context{Period: p, Filters: disclose.Filters{disclose.DimSex: "female"}})
	raw, err := EncodeRecord(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := EncodeRecord(got)
	if !bytes.Equal(raw, again) {
		t.Errorf("round trip changed the record:\n%s\n%s", raw, again)
	}
	if _, err := DecodeRecord(append(raw[:len(raw)-1], []byte(`,"Extra":1}`)...)); err == nil {
		t.Error("an unknown field was read")
	}
	small := bytes.Replace(raw, []byte(`"Patients":{"value":null,"suppressed":true}`), []byte(`"Patients":{"value":3,"suppressed":false}`), 1)
	if bytes.Equal(small, raw) {
		t.Fatal("the test proves nothing: no Patients field")
	}
	if _, err := DecodeRecord(small); err == nil {
		t.Error("a shown 3 was read")
	}
}

func mustDay(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}
