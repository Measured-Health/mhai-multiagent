// Package historytest is A7's attacker (Plan 7c spec §6.4, §11.5): it
// reads the JSON the routes send, and only that JSON, with the published
// definitions and the calendar, and writes every equation an attacker could:
// each shown cell, each shown 0 as zeros over its box, each hospital a
// period doesn't list as zeros, and every week before a hospital's
// onboarding as zeros. It imports neither the guard (smallgroup's joint.go)
// nor analytics.Body: it parses JSON as maps, by the keys the contract
// names, so a guard bug can't hide in a shared model.
package historytest

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// The published definitions (spec §5, the contract): the base categories
// of a hospital-week, and each published count as a set of them.
const (
	qgCats = 7 // pass, pass degraded, regenerate, regenerate degraded, escalate, escalate degraded, unanswered
	rCats  = 2 // agreed, corrected
	// MaxHospitals bounds the hospitals of one history.
	MaxHospitals = 24
)

type measure struct {
	family int // 0 QG, 1 R
	cats   []int
}

var measures = map[string]measure{
	"questions":  {0, []int{0, 1, 2, 3, 4, 5, 6}},
	"answers":    {0, []int{0, 1, 2, 3, 4, 5}},
	"pass":       {0, []int{0, 1}},
	"regenerate": {0, []int{2, 3}},
	"escalate":   {0, []int{4, 5}},
	"degraded":   {0, []int{1, 3, 5}},
	"reviews":    {1, []int{0, 1}},
	// Known through agreement_rate × reviews (below).
	"agreed":    {1, []int{0}},
	"corrected": {1, []int{1}},
}

// shares are numerator, denominator: a shown share with a shown
// denominator gives the numerator.
var shares = map[string][2]string{
	"pass_share":       {"pass", "answers"},
	"regenerate_share": {"regenerate", "answers"},
	"escalate_share":   {"escalate", "answers"},
	"agreement_rate":   {"agreed", "reviews"},
}

// sets are the sets that add up within a row: total, parts, and virtual
// parts (category sets never published) of the total's family.
var rowSets = []struct {
	total   string
	parts   []string
	virtual [][]int
}{
	{"answers", []string{"pass", "regenerate", "escalate"}, nil},
	{"questions", []string{"answers"}, [][]int{{6}}},
	{"answers", []string{"degraded"}, [][]int{{0, 2, 4}}},
	{"reviews", []string{"agreed", "corrected"}, nil},
}

// Parser collects documents.
type Parser struct {
	weeks   []calendar.Period
	weekIdx map[time.Time]int
	hosp    map[string]int
	onboard map[string]time.Time
	docs    []doc
	// unavailable counts analytics areas sent {unavailable: true}.
	unavailable int
	// changes counts the changes the analytics responses sent, each checked.
	changes int
	// Epoch is the release epoch (public: the notice names it), which
	// fixes the first period a not-yet response names.
	Epoch time.Time
}

type cell struct {
	vars  []int
	shown bool
	value int64 // as shown (0 when hidden)
}

type doc struct {
	source string // a label, so parent sets join cells of one route
	period calendar.Period
	raw    map[string]map[string]json.RawMessage // row → published key → value, as sent
	names  map[string]string                     // hospital id → name, as sent
	listed []string
	cells  map[string]map[string]int // row ("all" or id) → measure → index into cellsAll
	all    []cell
}

// New is a parser over the history's weeks, with each hospital's
// onboarding (public: nothing exists before it).
func New(weeks []calendar.Period, onboard map[string]time.Time) *Parser {
	p := &Parser{weeks: weeks, weekIdx: map[time.Time]int{}, hosp: map[string]int{}, onboard: onboard}
	for i, w := range weeks {
		p.weekIdx[w.From] = i
	}
	ids := make([]string, 0, len(onboard))
	for id := range onboard {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p.hosp[id] = len(p.hosp)
	}
	return p
}

// Var is the unknown of (family, hospital, week, category).
func (p *Parser) Var(family, h, w, c int) int {
	if family == 0 {
		return (h*len(p.weeks)+w)*qgCats + c
	}
	return MaxHospitals*len(p.weeks)*qgCats + (h*len(p.weeks)+w)*rCats + c
}

func (p *Parser) nVars() int { return MaxHospitals * len(p.weeks) * (qgCats + rCats) }

// HospitalIndex is a hospital's index, for the ground truth.
func (p *Parser) HospitalIndex(id string) (int, bool) { i, ok := p.hosp[id]; return i, ok }

func (p *Parser) slots(per calendar.Period) ([]int, error) {
	var out []int
	for w := calendar.Of(calendar.Week, per.From); w.From.Before(per.To); w = calendar.Next(w) {
		i, ok := p.weekIdx[w.From]
		if !ok {
			return nil, fmt.Errorf("week %v outside the history", w)
		}
		out = append(out, i)
	}
	return out, nil
}

func (p *Parser) box(m measure, hosps []int, slots []int, cats []int) []int {
	var out []int
	for _, h := range hosps {
		for _, w := range slots {
			for _, c := range cats {
				out = append(out, p.Var(m.family, h, w, c))
			}
		}
	}
	return out
}

type guarded struct {
	Value      *float64 `json:"value"`
	Suppressed bool     `json:"suppressed"`
}

// Add reads one calendar response (a stored body or GET /platform/guardrails
// for week, month or quarter) for the period per.
func (p *Parser) Add(source string, raw []byte, per calendar.Period) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	var hospitals []map[string]json.RawMessage
	if err := json.Unmarshal(top["hospitals"], &hospitals); err != nil {
		return err
	}
	var allRow map[string]json.RawMessage
	if err := json.Unmarshal(top["all"], &allRow); err != nil {
		return err
	}
	return p.addDoc(source, per, allRow, hospitals)
}

// addDoc adds one document's rows: all (nil when the route sends none) and
// one per listed hospital, each a map from the published key to its
// {value, suppressed}.
func (p *Parser) addDoc(source string, per calendar.Period, allRow map[string]json.RawMessage, hospitals []map[string]json.RawMessage) error {
	slots, err := p.slots(per)
	if err != nil {
		return err
	}
	d := doc{source: source, period: per, cells: map[string]map[string]int{}, raw: map[string]map[string]json.RawMessage{}, names: map[string]string{}}
	rows := map[string]map[string]json.RawMessage{}
	if allRow != nil {
		rows["all"] = allRow
	}
	var listedIdx []int
	for _, h := range hospitals {
		var ref struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(h["hospital"], &ref); err != nil {
			return err
		}
		i, ok := p.hosp[ref.ID]
		if !ok {
			return fmt.Errorf("an unknown hospital %s", ref.ID)
		}
		if _, dup := rows[ref.ID]; dup {
			return fmt.Errorf("hospital %s listed twice", ref.ID)
		}
		d.listed = append(d.listed, ref.ID)
		d.names[ref.ID] = ref.Name
		listedIdx = append(listedIdx, i)
		rows[ref.ID] = h
	}
	d.raw = rows
	for rowID, row := range rows {
		hosps := listedIdx
		if rowID != "all" {
			hosps = []int{p.hosp[rowID]}
		}
		vals := map[string]guarded{}
		for k, v := range row {
			if _, isMeasure := measures[k]; !isMeasure {
				if _, isShare := shares[k]; !isShare {
					continue
				}
			}
			var g guarded
			if err := json.Unmarshal(v, &g); err != nil {
				return fmt.Errorf("%s.%s: %w", rowID, k, err)
			}
			vals[k] = g
		}
		d.cells[rowID] = map[string]int{}
		add := func(name string, shown bool, value int64) {
			m := measures[name]
			d.cells[rowID][name] = len(d.all)
			d.all = append(d.all, cell{vars: p.box(m, hosps, slots, m.cats), shown: shown, value: value})
		}
		for name, g := range vals {
			if _, ok := measures[name]; !ok {
				continue
			}
			v := int64(0)
			if g.Value != nil {
				v = int64(math.Round(*g.Value))
			}
			add(name, g.Value != nil && !g.Suppressed, v)
		}
		// Shares: a shown share over a shown denominator gives its
		// numerator; agreed and corrected exist only that way (or as zeros
		// under a shown review count of 0).
		if r, ok := vals["reviews"]; ok {
			rate := vals["agreement_rate"]
			known := (rate.Value != nil && r.Value != nil) || (r.Value != nil && *r.Value == 0)
			var agreed int64
			if rate.Value != nil && r.Value != nil {
				agreed = int64(math.Round(*rate.Value * *r.Value))
			}
			reviews := int64(0)
			if r.Value != nil {
				reviews = int64(math.Round(*r.Value))
			}
			add("agreed", known, agreed)
			add("corrected", known, reviews-agreed)
		}
		for name, nd := range shares {
			g, ok := vals[name]
			if !ok || g.Value == nil || name == "agreement_rate" {
				continue
			}
			den, ok := vals[nd[1]]
			if !ok || den.Value == nil {
				continue
			}
			if i, ok := d.cells[rowID][nd[0]]; ok && !d.all[i].shown {
				d.all[i].shown = true // the share gave it away
				d.all[i].value = int64(math.Round(*g.Value * *den.Value))
			}
		}
	}
	p.docs = append(p.docs, d)
	return nil
}

// AddRolling reads a deprecated rolling response: every count and share
// must be hidden (R41a-3), so it adds no equation.
func (p *Parser) AddRolling(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	var hospitals []map[string]json.RawMessage
	if err := json.Unmarshal(top["hospitals"], &hospitals); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(top["all"], &all); err != nil {
		return err
	}
	for i, row := range append([]map[string]json.RawMessage{all}, hospitals...) {
		for k, v := range row {
			if k == "hospital" {
				continue
			}
			var g guarded
			if err := json.Unmarshal(v, &g); err != nil {
				return err
			}
			if g.Value != nil || !g.Suppressed {
				return fmt.Errorf("rolling row %d: %s is %s", i, k, v)
			}
		}
	}
	return nil
}

// Report is what the solver exposed.
type Report struct {
	SmallCells, SmallSums []string
	ExposedCells          int
	Cells, Hidden         int
}

// Check runs the exact solver over every document at once. value is the
// ground truth of a box (the test's, never the parser's).
func (p *Parser) Check(value func(vars []int) int64) Report {
	var eqs [][]int
	zero := func(vs []int) {
		for _, v := range vs {
			eqs = append(eqs, []int{v})
		}
	}
	// Onboarding: every week that ended at or before it.
	for id, h := range p.hosp {
		for w, wk := range p.weeks {
			if !wk.To.After(p.onboard[id]) {
				for f, n := range []int{qgCats, rCats} {
					for c := 0; c < n; c++ {
						zero([]int{p.Var(f, h, w, c)})
					}
				}
			}
		}
	}
	type protected struct {
		vars []int
		cell bool
		desc string
	}
	var prot []protected
	var rep Report
	for di, d := range p.docs {
		slots, _ := p.slots(d.period)
		for _, c := range d.all {
			rep.Cells++
			switch {
			case !c.shown:
				rep.Hidden++
				prot = append(prot, protected{c.vars, true, fmt.Sprintf("%s %v hidden cell", d.source, d.period)})
			case c.value == 0:
				zero(c.vars)
			default:
				eqs = append(eqs, c.vars)
			}
		}
		listed := map[string]bool{}
		for _, id := range d.listed {
			listed[id] = true
		}
		for id, h := range p.hosp {
			if !listed[id] {
				for _, w := range slots {
					for f, n := range []int{qgCats, rCats} {
						for c := 0; c < n; c++ {
							zero([]int{p.Var(f, h, w, c)})
						}
					}
				}
			}
		}
		// Sets: rows, columns, parent sets.
		addSet := func(members []cell, desc string) {
			var vs []int
			for _, m := range members {
				if !m.shown {
					vs = append(vs, m.vars...)
				}
			}
			if len(vs) > 0 {
				prot = append(prot, protected{vs, false, desc})
			}
		}
		for rowID, cells := range d.cells {
			hosps := []int{}
			if rowID == "all" {
				for _, id := range d.listed {
					hosps = append(hosps, p.hosp[id])
				}
			} else {
				hosps = append(hosps, p.hosp[rowID])
			}
			for _, rs := range rowSets {
				if _, ok := cells[rs.total]; !ok {
					continue
				}
				var mem []cell
				ok := true
				for _, part := range rs.parts {
					i, has := cells[part]
					if !has {
						ok = false
						break
					}
					mem = append(mem, d.all[i])
				}
				if !ok {
					continue
				}
				fam := measures[rs.total]
				for _, vc := range rs.virtual {
					mem = append(mem, cell{vars: p.box(fam, hosps, slots, vc)})
				}
				addSet(mem, fmt.Sprintf("%s %v row %s set %s", d.source, d.period, rowID, rs.total))
			}
		}
		for name := range measures {
			if _, ok := d.cells["all"][name]; !ok {
				continue
			}
			var mem []cell
			for _, id := range d.listed {
				if i, ok := d.cells[id][name]; ok {
					mem = append(mem, d.all[i])
				}
			}
			addSet(mem, fmt.Sprintf("%s %v column %s", d.source, d.period, name))
		}
		children := calendar.Children(d.period)
		if len(children) == 0 {
			continue
		}
		var kids []doc
		for _, c := range children {
			for _, o := range p.docs[:di] {
				if o.source == d.source && o.period.Equal(c) {
					kids = append(kids, o)
				}
			}
		}
		if len(kids) != len(children) {
			continue
		}
		for rowID, cells := range d.cells {
			for name := range cells {
				var mem []cell
				for _, k := range kids {
					if i, ok := k.cells[rowID][name]; ok {
						mem = append(mem, k.all[i])
					}
				}
				addSet(mem, fmt.Sprintf("%s %v parent set %s %s", d.source, d.period, rowID, name))
			}
		}
	}
	boxes := make([][]int, len(prot))
	for i, pr := range prot {
		boxes[i] = pr.vars
	}
	ex := solve.Exposed(p.nVars(), eqs, boxes)
	for i, e := range ex {
		if !e {
			continue
		}
		v := value(prot[i].vars)
		small := v >= 1 && v <= 4
		switch {
		case prot[i].cell:
			rep.ExposedCells++
			if small {
				rep.SmallCells = append(rep.SmallCells, fmt.Sprintf("%s = %d", prot[i].desc, v))
			}
		case small:
			rep.SmallSums = append(rep.SmallSums, fmt.Sprintf("%s = %d", prot[i].desc, v))
		}
	}
	return rep
}

// Decode splits a variable back into (family, hospital, week, category),
// for the ground truth.
func (p *Parser) Decode(v int) (family, h, w, c int) {
	n0 := MaxHospitals * len(p.weeks) * qgCats
	if v < n0 {
		return 0, v / qgCats / len(p.weeks), (v / qgCats) % len(p.weeks), v % qgCats
	}
	v -= n0
	return 1, v / rCats / len(p.weeks), (v / rCats) % len(p.weeks), v % rCats
}
