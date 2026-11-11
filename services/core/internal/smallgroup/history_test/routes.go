package historytest

// The analytics routes' JSON (Plan 7c Tasks 6 and 7, spec §7.1, §7.2), the
// attacker's side: GET /platform/analytics and the hospital series, read as
// JSON maps by the contract's keys, with this package's own code. Every
// shown cell becomes the parser's equations as the other routes' do; and
// each response is checked against what the guard and the history check
// validated, the stored body of the same period:
//
//   - every row cell is the stored cell, shown and hidden alike;
//   - the rows are the stored body's named hospitals;
//   - every median is the median of that response's shown values (a count
//     median strictly between 0 and 5 hidden);
//   - every trend and series point is one period's stored cell, for each
//     stored period of the latest TrendLength and no other (no total
//     across periods, R41a-2), and its median the median of the other
//     hospitals' shown values in that stored body.
//
// What the route may compute is only ever from shown values, so a response
// that passes adds nothing the stored bodies don't hold.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// trendLength is the contract's: 12 weeks, 12 months, 8 quarters.
var trendLength = map[calendar.Kind]int{calendar.Week: 12, calendar.Month: 12, calendar.Quarter: 8}

const stampLayout = "2006-01-02T15:04:05.000Z"

// storedOfKind is every stored document of kind k, oldest first.
func (p *Parser) storedOfKind(k calendar.Kind) []*doc {
	var out []*doc
	for i := range p.docs {
		if p.docs[i].source == "stored" && p.docs[i].period.Kind == k {
			out = append(out, &p.docs[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].period.From.Before(out[j].period.From) })
	return out
}

// window is the stored documents of kind k a trend or series ending with
// the newest covers, oldest first.
func (p *Parser) window(k calendar.Kind) []*doc {
	all := p.storedOfKind(k)
	if len(all) == 0 {
		return nil
	}
	newest := all[len(all)-1].period
	oldest := newest
	for i := 1; i < trendLength[k]; i++ {
		oldest = calendar.Prev(oldest)
	}
	var out []*doc
	for _, d := range all {
		if !d.period.From.Before(oldest.From) {
			out = append(out, d)
		}
	}
	return out
}

func readGuarded(raw json.RawMessage) (guarded, error) {
	var g guarded
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return guarded{}, fmt.Errorf("not {value, suppressed}: %s", raw)
	}
	if g.Suppressed && g.Value != nil {
		return guarded{}, fmt.Errorf("hidden with a value: %s", raw)
	}
	return g, nil
}

func sameGuarded(a, b json.RawMessage) bool {
	x, err1 := readGuarded(a)
	y, err2 := readGuarded(b)
	if err1 != nil || err2 != nil || x.Suppressed != y.Suppressed || (x.Value == nil) != (y.Value == nil) {
		return false
	}
	return x.Value == nil || *x.Value == *y.Value
}

func shownValue(raw json.RawMessage) (float64, bool) {
	g, err := readGuarded(raw)
	if err != nil || g.Suppressed || g.Value == nil {
		return 0, false
	}
	return *g.Value, true
}

func mid(v []float64) (float64, bool) {
	if len(v) == 0 {
		return 0, false
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2], true
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2, true
}

func roundTo(v float64, digits int) float64 {
	p := math.Pow10(digits)
	return math.Round(v*p) / p
}

// medianKinds is how each median is written: digits, or -1 for a count
// (hidden strictly between 0 and 5).
var medianKinds = map[string]int{
	"questions": -1, "active_clinicians": -1, "answers": -1, "reviews": -1, "pass": -1, "regenerate": -1, "escalate": -1,
	"questions_per_clinician": 2, "escalate_share": 4, "agreement_rate": 4, "median_review_hours": 1,
}

// checkMedian: got is the median the route sent for the shown values vals.
func checkMedian(what string, got json.RawMessage, vals []float64, digits int) error {
	g, err := readGuarded(got)
	if err != nil {
		return fmt.Errorf("%s median: %w", what, err)
	}
	m, has := mid(vals)
	switch {
	case !has:
		if g.Value != nil || g.Suppressed {
			return fmt.Errorf("%s median %s over no shown value", what, got)
		}
	case digits < 0 && m > 0 && m < 5:
		if g.Value != nil || !g.Suppressed {
			return fmt.Errorf("%s: a count median of %v sent as %s", what, m, got)
		}
	case digits >= 0:
		if g.Value == nil || *g.Value != roundTo(m, digits) {
			return fmt.Errorf("%s median %s, the shown values give %v", what, got, roundTo(m, digits))
		}
	default:
		if g.Value == nil || *g.Value != m {
			return fmt.Errorf("%s median %s, the shown values give %v", what, got, m)
		}
	}
	return nil
}

// strictObject reads an object with exactly the keys given.
func strictObject(raw json.RawMessage, keys ...string) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("not an object: %s", raw)
	}
	if len(m) != len(keys) {
		return nil, fmt.Errorf("keys of %s, want %v", raw, keys)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("no %s in %s", k, raw)
		}
	}
	return m, nil
}

func isUnavailable(raw json.RawMessage) bool {
	m, err := strictObject(raw, "unavailable")
	return err == nil && string(m["unavailable"]) == "true"
}

// Unavailable counts the areas the analytics responses sent unavailable.
func (p *Parser) Unavailable() int { return p.unavailable }

// AddAnalytics reads one GET /platform/analytics response.
func (p *Parser) AddAnalytics(raw []byte) error {
	top, err := strictObject(raw, "period", "from", "to", "changes", "use", "alerts", "quality", "hospitals", "what_changed")
	if err != nil {
		return err
	}
	var kind string
	if err := json.Unmarshal(top["period"], &kind); err != nil {
		return err
	}
	k, err := calendar.ParseKind(kind)
	if err != nil {
		return err
	}
	if string(top["alerts"]) != "null" || !isUnavailable(top["changes"]) {
		return fmt.Errorf("alerts %s, changes %s", top["alerts"], top["changes"])
	}
	if !isUnavailable(top["what_changed"]) {
		if err := p.AddChanges(top["what_changed"]); err != nil {
			return fmt.Errorf("what_changed: %w", err)
		}
	}
	var from, to *string
	_ = json.Unmarshal(top["from"], &from)
	_ = json.Unmarshal(top["to"], &to)
	if from == nil || to == nil {
		if from != nil || to != nil {
			return fmt.Errorf("one bound null: %s", raw)
		}
		if n := len(p.storedOfKind(k)); n > 0 {
			return fmt.Errorf("%s: not yet, with %d stored", k, n)
		}
		for _, a := range []string{"use", "quality", "hospitals"} {
			m, err := strictObject(top[a], "not_yet", "first_from")
			if err != nil || string(m["not_yet"]) != "true" {
				return fmt.Errorf("not yet: %s %s", a, top[a])
			}
			// The first period of k that starts at or after the release
			// epoch (R41a-3), worked out here from the calendar alone.
			if p.Epoch.IsZero() {
				return fmt.Errorf("not yet, and the parser has no epoch")
			}
			first := calendar.Of(k, p.Epoch)
			if first.From.Before(p.Epoch) {
				first = calendar.Next(first)
			}
			var ff string
			_ = json.Unmarshal(m["first_from"], &ff)
			t, err := time.Parse(stampLayout, ff)
			if err != nil || !t.Equal(first.From) {
				return fmt.Errorf("first_from %s, want %v", m["first_from"], first)
			}
		}
		return nil
	}
	f, err1 := time.Parse(stampLayout, *from)
	e, err2 := time.Parse(stampLayout, *to)
	per := calendar.Of(k, f)
	if err1 != nil || err2 != nil || !per.From.Equal(f) || !per.To.Equal(e) {
		return fmt.Errorf("bounds %v %v", *from, *to)
	}
	win := p.window(k)
	if len(win) == 0 || !win[len(win)-1].period.Equal(per) {
		return fmt.Errorf("%v is not the newest stored %s", per, k)
	}
	st := win[len(win)-1]
	rows := map[string]map[string]json.RawMessage{}
	names := map[string]string{}
	put := func(id, key string, v json.RawMessage) error {
		if rows[id] == nil {
			rows[id] = map[string]json.RawMessage{}
		}
		if old, ok := rows[id][key]; ok && !sameGuarded(old, v) {
			return fmt.Errorf("%s %s sent as %s and %s", id, key, old, v)
		}
		rows[id][key] = v
		return nil
	}
	type area struct {
		name    string
		rowKeys []string
		keyMap  map[string]string // sent key → stored key
		trend   string            // the stored all cell the trend shows
	}
	areas := []area{
		{"use", []string{"hospital", "questions", "active_clinicians", "questions_per_clinician"},
			map[string]string{"questions": "questions", "active_clinicians": "active_clinicians", "questions_per_clinician": "questions_per_clinician"}, "questions"},
		{"quality", []string{"hospital", "answers", "needs_review_share", "confidence", "reviews", "agreement_rate", "median_review_hours"},
			map[string]string{"answers": "answers", "needs_review_share": "escalate_share", "high": "pass", "medium": "regenerate", "low": "escalate",
				"reviews": "reviews", "agreement_rate": "agreement_rate", "median_review_hours": "median_review_hours"}, "escalate_share"},
		{"hospitals", []string{"hospital", "size"}, map[string]string{"staff": "staff", "active_clinicians": "active_clinicians", "questions": "questions"}, ""},
	}
	for _, a := range areas {
		if isUnavailable(top[a.name]) {
			p.unavailable++
			continue
		}
		keys := []string{"updated_at", "rows", "median", "trend"}
		if a.trend == "" {
			keys = []string{"updated_at", "rows"}
		}
		obj, err := strictObject(top[a.name], keys...)
		if err != nil {
			return fmt.Errorf("%s: %w", a.name, err)
		}
		var list []json.RawMessage
		if err := json.Unmarshal(obj["rows"], &list); err != nil {
			return err
		}
		shown := map[string][]float64{}
		seen := map[string]bool{}
		for _, x := range list {
			row, err := strictObject(x, a.rowKeys...)
			if err != nil {
				return fmt.Errorf("%s row: %w", a.name, err)
			}
			var ref struct{ ID, Name string }
			if err := json.Unmarshal(row["hospital"], &ref); err != nil {
				return err
			}
			if seen[ref.ID] {
				return fmt.Errorf("%s: %s twice", a.name, ref.ID)
			}
			seen[ref.ID] = true
			names[ref.ID] = ref.Name
			flat := map[string]json.RawMessage{}
			for _, key := range a.rowKeys[1:] {
				switch key {
				case "confidence", "size":
					inner := []string{"high", "medium", "low"}
					if key == "size" {
						inner = []string{"staff", "active_clinicians", "questions"}
					}
					m, err := strictObject(row[key], inner...)
					if err != nil {
						return fmt.Errorf("%s %s: %w", a.name, key, err)
					}
					for ik, v := range m {
						flat[ik] = v
					}
				default:
					flat[key] = row[key]
				}
			}
			for sent, v := range flat {
				storedKey := a.keyMap[sent]
				if err := put(ref.ID, storedKey, v); err != nil {
					return err
				}
				sr, ok := st.raw[ref.ID]
				if !ok || !sameGuarded(v, sr[storedKey]) {
					return fmt.Errorf("%v %s %s: sent %s, stored %s", per, ref.ID, sent, v, sr[storedKey])
				}
				if x, ok := shownValue(v); ok {
					shown[sent] = append(shown[sent], x)
				}
			}
		}
		// The rows are exactly the stored body's named hospitals.
		for id, name := range st.names {
			if name != "" && !seen[id] {
				return fmt.Errorf("%s: stored hospital %s not sent", a.name, id)
			}
		}
		for id := range seen {
			if st.names[id] == "" || st.names[id] != names[id] {
				return fmt.Errorf("%s: hospital %s isn't a named stored one", a.name, id)
			}
		}
		if a.trend == "" {
			continue
		}
		med, err := strictObject(obj["median"], a.rowKeys[1:]...)
		if err != nil {
			return fmt.Errorf("%s median: %w", a.name, err)
		}
		for sent, v := range med {
			if sent == "confidence" {
				cm, err := strictObject(v, "high", "medium", "low")
				if err != nil {
					return err
				}
				for ik, iv := range cm {
					if err := checkMedian(a.name+" "+ik, iv, shown[ik], medianKinds[a.keyMap[ik]]); err != nil {
						return err
					}
				}
				continue
			}
			if err := checkMedian(a.name+" "+sent, v, shown[sent], medianKinds[a.keyMap[sent]]); err != nil {
				return err
			}
		}
		if err := checkPoints(obj["trend"], win, func(d *doc) json.RawMessage { return d.raw["all"][a.trend] }, nil); err != nil {
			return fmt.Errorf("%s trend: %w", a.name, err)
		}
	}
	// The equations: the rows as stored keys, no all row (the route sends
	// none).
	var hosp []map[string]json.RawMessage
	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		row := map[string]json.RawMessage{"hospital": json.RawMessage(fmt.Sprintf(`{"id":%q,"name":%q}`, id, names[id]))}
		for k, v := range rows[id] {
			row[k] = v
		}
		hosp = append(hosp, row)
	}
	return p.addDoc("analytics", per, nil, hosp)
}

// checkPoints: points are one per document of win, in order, each the
// cell cellOf gives (hidden alike); median, when given, checks each
// point's median.
func checkPoints(raw json.RawMessage, win []*doc, cellOf func(*doc) json.RawMessage, median func(*doc, *float64) error) error {
	var pts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pts); err != nil {
		return err
	}
	if len(pts) != len(win) {
		return fmt.Errorf("%d points for %d stored periods", len(pts), len(win))
	}
	for i, pt := range pts {
		keys := []string{"start", "value"}
		if median != nil {
			keys = append(keys, "median")
		}
		if _, err := strictObject(mustJSON(pt), keys...); err != nil {
			return err
		}
		var start string
		_ = json.Unmarshal(pt["start"], &start)
		if start != win[i].period.From.Format(stampLayout) {
			return fmt.Errorf("point %d starts %s, want %v", i, start, win[i].period)
		}
		want := cellOf(win[i])
		if want == nil || !sameGuarded(pt["value"], want) {
			return fmt.Errorf("point %d (%v): %s, stored %s", i, win[i].period, pt["value"], want)
		}
		if median != nil {
			var m *float64
			if err := json.Unmarshal(pt["median"], &m); err != nil {
				return err
			}
			if err := median(win[i], m); err != nil {
				return fmt.Errorf("point %d: %w", i, err)
			}
		}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// seriesMeasures: the stored key and the decimals of each served measure.
var seriesMeasures = map[string]struct {
	key    string
	digits int
}{"questions_per_clinician": {"questions_per_clinician", 2}, "needs_review_share": {"escalate_share", 4}}

// AddSeries reads one GET /platform/analytics/hospitals/{id}/series
// response of kind k. A series adds no equation of its own (its points are
// stored cells, checked here); it is checked against the stored bodies.
func (p *Parser) AddSeries(raw []byte, k calendar.Kind) error {
	top, err := strictObject(raw, "measure", "hospital", "points")
	if err != nil {
		return err
	}
	var measure string
	_ = json.Unmarshal(top["measure"], &measure)
	sm, ok := seriesMeasures[measure]
	if !ok {
		return fmt.Errorf("measure %q", measure)
	}
	var ref struct{ ID, Name string }
	if err := json.Unmarshal(top["hospital"], &ref); err != nil {
		return err
	}
	win := p.window(k)
	named := false
	for _, d := range win {
		if d.names[ref.ID] != "" {
			named = named || d.names[ref.ID] == ref.Name
		}
	}
	if !named {
		return fmt.Errorf("hospital %s %q is in no stored period by that name", ref.ID, ref.Name)
	}
	nothing := json.RawMessage(`{"value":null,"suppressed":false}`)
	cell := func(d *doc) json.RawMessage {
		if row, listed := d.raw[ref.ID]; listed {
			return row[sm.key]
		}
		return nothing
	}
	median := func(d *doc, got *float64) error {
		var vals []float64
		for id, row := range d.raw {
			if id == "all" || id == ref.ID || d.names[id] == "" {
				continue
			}
			if v, ok := shownValue(row[sm.key]); ok {
				vals = append(vals, v)
			}
		}
		m, has := mid(vals)
		switch {
		case !has && got != nil:
			return fmt.Errorf("median %v over no shown value", *got)
		case has && (got == nil || *got != roundTo(m, sm.digits)):
			return fmt.Errorf("median %v, the other hospitals' shown values give %v", got, roundTo(m, sm.digits))
		}
		return nil
	}
	return checkPoints(top["points"], win, cell, median)
}
