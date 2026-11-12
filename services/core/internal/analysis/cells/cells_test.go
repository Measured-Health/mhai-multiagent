package cells

import (
	"testing"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// The function's result decodes into one context's raw values: the series
// is every child period, each the sum of its blocks; an unknown field is
// refused (a migration and a build out of step fail closed).
func TestDecode(t *testing.T) {
	day, _ := time.Parse(time.DateOnly, "2026-10-01")
	p := calendar.Of(calendar.Month, day)
	body := `{"first": false, "patients": 9, "cases": 7, "new": 3,
		"by_condition": [{"c": "c1", "cases": 7, "new": 3}],
		"blocks": [{"start": "2026-10-01", "cases": 4, "new": 2}, {"start": "2026-10-22", "cases": 3, "new": 1}],
		"by_age": [{"k": "", "n": 9}], "by_sex": [{"k": "female", "n": 9}], "by_setting": [{"k": "admitted", "n": 9}],
		"by_location": [{"k": "L1", "setting": "admitted", "n": 9}], "by_specialty": [{"k": "", "n": 9}],
		"sources": ["S1", ""], "pairs": [], "findings": [],
		"alerts": {"total": 0, "critical": 0, "urgent": 0, "info": 0, "dismissed": 0, "reasons": [], "ack": [], "critical_ack": []},
		"quality": []}`
	raw, err := decode([]byte(body), p, disclose.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Series) != 4 || raw.Series[0].Cases != 4 || raw.Series[3].Cases != 3 || raw.Series[1].Cases != 0 {
		t.Errorf("series %+v", raw.Series)
	}
	if raw.Total.Existing != 4 || len(raw.Sources) != 2 || raw.ByLocation[0].Setting != "admitted" {
		t.Errorf("raw %+v", raw)
	}
	if _, err := decode([]byte(`{"patients": 1, "names": ["x"]}`), p, disclose.Filters{}); err != ErrShape {
		t.Errorf("an unknown field: %v", err)
	}
	if FiltersJSON(disclose.Filters{disclose.DimSex: "female"}) != `{"sex":"female"}` {
		t.Error("filters JSON")
	}
}
