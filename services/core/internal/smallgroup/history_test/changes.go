package historytest

// What changed and the change series (Plan 7c-3, spec §7.2, §7.3), the
// attacker's side: the changes area of GET /platform/analytics and GET
// /platform/analytics/changes/{id}/series, read as JSON maps by the
// contract's keys, with this package's own code and its own copy of the
// published thresholds (it imports neither the evaluator nor the guard).
// A change may rest only on stored, shown cells: each is checked against
// the stored bodies of the week and the month it compares, so a response
// that passes adds nothing the stored bodies don't hold, and no change is
// flagged on a hidden value:
//
//   - the window is a stored week and the month that finished before it;
//   - the row (a hospital the week names and the month lists, or all) and
//     the measure's area are the published ones; serious is false;
//   - current is the week's stored cell, shown; previous is the month's
//     stored cell, shown, per day and scaled to the week where published so;
//   - the minimum's bases are shown on both sides and at the minimum, and
//     the change reaches its threshold in the direction named;
//   - the summary holds the two values and nothing else: no other number,
//     no hospital name;
//   - the follow-up's keys; each id and each key once.
//
// A change series is the change's measure for its row, one stored period
// per point, with the other hospitals' median (none for all).

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// changeRule is a published change measure (spec §7.3, Q3).
type changeRule struct {
	area     string
	key      string // the stored key of the value
	base     string // the stored key the minimum is on
	min      float64
	change   float64 // a share of previous; points for shares
	share    bool
	perDay   bool
	prevDig  int // previous's decimals
	count    bool
	up, down string
}

var changeRules = map[string]changeRule{
	"questions":          {"use", "questions", "questions", 10, 0.5, false, true, 1, true, "rising", "falling"},
	"needs_review_share": {"quality", "escalate_share", "answers", 20, 0.15, true, false, 4, false, "worse", "better"},
	"agreement_rate":     {"quality", "agreement_rate", "reviews", 20, 0.15, true, false, 4, false, "better", "worse"},
}

var numberRe = regexp.MustCompile(`[0-9]+(\.[0-9]+)?`)

const eps = 1e-9

// Changes is the number of changes checked.
func (p *Parser) Changes() int { return p.changes }

// storedDoc is the stored document of period per.
func (p *Parser) storedDoc(per calendar.Period) *doc {
	for i := range p.docs {
		if p.docs[i].source == "stored" && p.docs[i].period.Equal(per) {
			return &p.docs[i]
		}
	}
	return nil
}

func stamp(raw json.RawMessage) (time.Time, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, err
	}
	return time.Parse(stampLayout, s)
}

// AddChanges checks one changes area.
func (p *Parser) AddChanges(raw json.RawMessage) error {
	top, err := strictObject(raw, "updated_at", "window", "items")
	if err != nil {
		return err
	}
	if _, err := stamp(top["updated_at"]); err != nil {
		return fmt.Errorf("updated_at %s", top["updated_at"])
	}
	win, err := strictObject(top["window"], "from", "to")
	if err != nil {
		return fmt.Errorf("window: %w", err)
	}
	from, err1 := stamp(win["from"])
	to, err2 := stamp(win["to"])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("window %s", top["window"])
	}
	week := calendar.Of(calendar.Week, to.Add(-time.Hour))
	month := calendar.Prev(calendar.Of(calendar.Month, week.From))
	if !week.To.Equal(to) || !month.From.Equal(from) {
		return fmt.Errorf("window %v–%v isn't a week and the month before it", from, to)
	}
	wd, md := p.storedDoc(week), p.storedDoc(month)
	if wd == nil {
		return fmt.Errorf("window week %v isn't stored", week)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(top["items"], &items); err != nil {
		return err
	}
	if len(items) > 0 && md == nil {
		return fmt.Errorf("changes against month %v, which isn't stored", month)
	}
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, it := range items {
		c, err := p.checkChange(it, week, month, wd, md)
		if err != nil {
			return err
		}
		if ids[c.id] || keys[c.key] {
			return fmt.Errorf("change %s or its key %s twice", c.id, c.key)
		}
		ids[c.id], keys[c.key] = true, true
		p.changes++
	}
	return nil
}

type checkedChange struct {
	id, key string
	measure string
}

// shownIn is a stored document's shown value of key in row, if any.
func shownIn(d *doc, row, key string) (float64, bool) {
	r, ok := d.raw[row]
	if !ok {
		return 0, false
	}
	return shownValue(r[key])
}

func (p *Parser) checkChange(raw json.RawMessage, week, month calendar.Period, wd, md *doc) (checkedChange, error) {
	it, err := strictObject(raw, "id", "hospital", "area", "measure", "direction", "serious", "previous", "current", "summary",
		"follow_up", "version")
	if err != nil {
		return checkedChange{}, fmt.Errorf("change: %w", err)
	}
	var c checkedChange
	var area, dir, summary string
	_ = json.Unmarshal(it["id"], &c.id)
	_ = json.Unmarshal(it["measure"], &c.measure)
	_ = json.Unmarshal(it["area"], &area)
	_ = json.Unmarshal(it["direction"], &dir)
	_ = json.Unmarshal(it["summary"], &summary)
	rule, ok := changeRules[c.measure]
	if !ok {
		return c, fmt.Errorf("measure %q", c.measure)
	}
	if area != rule.area {
		return c, fmt.Errorf("%s in area %q", c.measure, area)
	}
	if string(it["serious"]) != "false" {
		return c, fmt.Errorf("serious %s", it["serious"])
	}
	if dir != rule.up && dir != rule.down {
		return c, fmt.Errorf("%s direction %q isn't the measure's", c.measure, dir)
	}
	var version int
	if err := json.Unmarshal(it["version"], &version); err != nil || version < 1 {
		return c, fmt.Errorf("version %s", it["version"])
	}
	row := "all"
	if string(it["hospital"]) != "null" {
		var ref struct{ ID, Name string }
		if err := json.Unmarshal(it["hospital"], &ref); err != nil {
			return c, err
		}
		if wd.names[ref.ID] == "" || wd.names[ref.ID] != ref.Name {
			return c, fmt.Errorf("hospital %s %q isn't named in week %v", ref.ID, ref.Name, week)
		}
		if _, listed := md.raw[ref.ID]; !listed {
			return c, fmt.Errorf("hospital %s has no previous month %v", ref.ID, month)
		}
		row = ref.ID
	}
	c.key = row + "/" + c.measure + "/" + dir
	// current: the week's stored cell, shown.
	cur, ok := shownValue(it["current"])
	if !ok {
		return c, fmt.Errorf("current %s not shown", it["current"])
	}
	if sv, ok := shownIn(wd, row, rule.key); !ok || math.Abs(sv-cur) > eps {
		return c, fmt.Errorf("current %v, the week's stored cell %s", cur, wd.raw[row][rule.key])
	}
	// previous: the month's stored cell, shown, per day where published so.
	prev, ok := shownValue(it["previous"])
	if !ok {
		return c, fmt.Errorf("previous %s not shown", it["previous"])
	}
	mv, ok := shownIn(md, row, rule.key)
	if !ok {
		return c, fmt.Errorf("previous over the month's cell %s, which isn't shown", md.raw[row][rule.key])
	}
	want := mv
	if rule.perDay {
		// Per day, times the week's days, rounded half up to previous's
		// decimals: in whole units of the stored value's decimals, exactly.
		src := 0
		if !rule.count {
			src = 2
		}
		days := func(per calendar.Period) int64 { return int64(per.To.Sub(per.From) / (24 * time.Hour)) }
		v := int64(math.Round(mv * math.Pow10(src)))
		num := v * int64(math.Pow10(rule.prevDig-src)) * days(week)
		den := days(month)
		want = float64((2*num+den)/(2*den)) / math.Pow10(rule.prevDig)
	}
	if math.Abs(want-prev) > eps {
		return c, fmt.Errorf("previous %v, the month's stored cell gives %v", prev, want)
	}
	// The minimum: its bases shown on both sides and at the minimum.
	wb, ok1 := shownIn(wd, row, rule.base)
	mb, ok2 := shownIn(md, row, rule.base)
	if !ok1 || !ok2 || wb < rule.min || mb < rule.min || (rule.base == rule.key && (cur < rule.min-eps || prev < rule.min-eps)) {
		return c, fmt.Errorf("%s under the minimum: bases %s and %s", c.measure, wd.raw[row][rule.base], md.raw[row][rule.base])
	}
	// The threshold and the direction.
	diff := cur - prev
	if rule.share {
		if math.Abs(diff) < rule.change-eps {
			return c, fmt.Errorf("%s moved %v points, under the threshold", c.measure, diff)
		}
	} else if prev <= 0 || math.Abs(diff) < rule.change*prev-eps {
		return c, fmt.Errorf("%s moved %v from %v, under the threshold", c.measure, diff, prev)
	}
	if (diff > 0 && dir != rule.up) || (diff < 0 && dir != rule.down) || diff == 0 {
		return c, fmt.Errorf("%s direction %q for %v → %v", c.measure, dir, prev, cur)
	}
	// The summary: the two values, nothing else, no hospital's name.
	for _, d := range p.docs {
		for _, n := range d.names {
			if n != "" && strings.Contains(summary, n) {
				return c, fmt.Errorf("summary %q names a hospital", summary)
			}
		}
	}
	nums := numberRe.FindAllString(summary, -1)
	scale := 1.0
	if rule.share {
		scale = 100
	}
	if len(nums) != 2 {
		return c, fmt.Errorf("summary %q: %d numbers", summary, len(nums))
	}
	for i, v := range []float64{prev, cur} {
		x, err := strconv.ParseFloat(nums[i], 64)
		if err != nil || math.Abs(x-v*scale) > 1e-6 {
			return c, fmt.Errorf("summary %q: %s isn't %v", summary, nums[i], v*scale)
		}
	}
	// The follow-up's keys.
	if string(it["follow_up"]) != "null" {
		fu, err := strictObject(it["follow_up"], "by", "at", "note", "by_you", "version")
		if err != nil {
			return c, fmt.Errorf("follow_up: %w", err)
		}
		if _, err := strictObject(fu["by"], "id", "name"); err != nil {
			return c, fmt.Errorf("follow_up by: %w", err)
		}
	}
	return c, nil
}

// AddChangeSeries checks one GET /platform/analytics/changes/{id}/series
// response of kind k for the change item (as the changes area sent it). A
// series adds no equation: its points are stored cells.
func (p *Parser) AddChangeSeries(raw []byte, k calendar.Kind, item json.RawMessage) error {
	top, err := strictObject(raw, "change_id", "measure", "hospital", "points")
	if err != nil {
		return err
	}
	it, err := strictObject(item, "id", "hospital", "area", "measure", "direction", "serious", "previous", "current", "summary",
		"follow_up", "version")
	if err != nil {
		return fmt.Errorf("the change: %w", err)
	}
	var measure, want string
	_ = json.Unmarshal(top["measure"], &measure)
	_ = json.Unmarshal(it["measure"], &want)
	rule, ok := changeRules[measure]
	if !ok || measure != want {
		return fmt.Errorf("series measure %q for a change of %q", measure, want)
	}
	if !jsonEqual(top["change_id"], it["id"]) {
		return fmt.Errorf("series change_id %s for change %s", top["change_id"], it["id"])
	}
	if !jsonEqual(top["hospital"], it["hospital"]) {
		return fmt.Errorf("series hospital %s for a change of %s", top["hospital"], it["hospital"])
	}
	row := "all"
	if string(top["hospital"]) != "null" {
		var ref struct{ ID, Name string }
		if err := json.Unmarshal(top["hospital"], &ref); err != nil {
			return err
		}
		row = ref.ID
	}
	nothing := json.RawMessage(`{"value":null,"suppressed":false}`)
	cell := func(d *doc) json.RawMessage {
		if r, listed := d.raw[row]; listed {
			return r[rule.key]
		}
		return nothing
	}
	digits := rule.prevDig
	if rule.count {
		digits = -1
	}
	median := func(d *doc, got *float64) error {
		if row == "all" {
			if got != nil {
				return fmt.Errorf("a median %v for all hospitals", *got)
			}
			return nil
		}
		var vals []float64
		for id, r := range d.raw {
			if id == "all" || id == row || d.names[id] == "" {
				continue
			}
			if v, ok := shownValue(r[rule.key]); ok {
				vals = append(vals, v)
			}
		}
		m, has := mid(vals)
		switch {
		case !has || (digits < 0 && m > 0 && m < 5):
			if got != nil {
				return fmt.Errorf("median %v over no shown value or a hidden count median", *got)
			}
		case got == nil:
			return fmt.Errorf("median null, the other hospitals' shown values give %v", m)
		case digits >= 0 && *got != roundTo(m, digits), digits < 0 && *got != m:
			return fmt.Errorf("median %v, the other hospitals' shown values give %v", *got, m)
		}
		return nil
	}
	return checkPoints(top["points"], p.window(k), cell, median)
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return string(mustJSON(x)) == string(mustJSON(y))
}
