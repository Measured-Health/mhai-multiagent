// Package attack is the disclosure tests' attacker (design §5.7; plan
// Tasks 6–8): it reads the JSON bodies Analysis would send, and only that
// JSON, with the published definitions and the calendar, and writes every
// equation an attacker could, over the atoms of a synthetic world: the
// full cross-classification of its patients and facts. It imports neither
// the guard (internal/analysis/disclose) nor smallgroup's modular check:
// its solver is smallgroup/solve, exact over the rationals, shared with
// 7c's attacker. Nothing here is, or derives from, a real patient.
package attack

import (
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
)

// Location is a location id and its setting.
type Location struct{ ID, Setting string }

// Dims are a world's dictionaries: every value an attribute may take
// ("" is null where the contract allows it). The attacker's universe is
// every combination of them.
type Dims struct {
	Conditions  []string
	Ages        []string
	Sexes       []string
	Locations   []Location
	Specialties []string
	Sources     []string
	Findings    []Finding
}

// Finding is a finding's name and flag.
type Finding struct{ Name, Flag string }

// The contract's closed lists, the attacker's copy.
var (
	confidences = []string{"high", "caution", "review", "failed"}
	ratings     = []string{"", "agreed", "not_agreed"}
	corrections = []string{"", "approved", "rejected", "waiting"}
	severities  = []string{"critical", "urgent", "info"}
	dismissals  = []string{"", "not_relevant", "already_known", "wrong_patient", "other"}
)

// Consult is one consultation: its condition, confidence, rating and
// correction ("" none).
type Consult struct {
	Cond                           int
	Confidence, Rating, Correction int
}

// Alert is one alert: severity, dismissal ("" kept) and the minutes to
// acknowledge (−1 not acknowledged).
type Alert struct {
	Severity, Dismiss int
	Ack               int
}

// Block is one patient's facts in one block: the conditions with a case,
// those new (first ever), the findings with their results, consultations
// and alerts.
type Block struct {
	Conds, New, Finds uint32
	Results           map[int]int
	Consults          []Consult
	Alerts            []Alert
}

// Subject is one synthetic patient: fixed attributes, and facts per block
// (indexes into World.Blocks).
type Subject struct {
	Age, Sex, Loc, Spec, Source int
	Blocks                      map[int]*Block
}

// World is a synthetic hospital: its blocks (consecutive month-aligned
// weeks), whether its history starts at the first block (nothing before:
// the attacker knows it), and its patients.
type World struct {
	Dims     Dims
	Blocks   []calendar.Period
	Start    bool
	Subjects []Subject
}

// BlockIndex is the index of the block starting at p's first day, or −1.
func (w *World) BlockIndex(from string) int {
	for i, b := range w.Blocks {
		if b.From.Format("2006-01-02") == from {
			return i
		}
	}
	return -1
}

// Blocks lists the month-aligned weeks from the block containing from to
// the one containing to (inclusive).
func Blocks(from, to calendar.Period) []calendar.Period {
	var out []calendar.Period
	for b := calendar.Of(calendar.Week, from.From); !b.From.After(to.From); b = calendar.Next(b) {
		out = append(out, b)
	}
	return out
}

// Options shape a generated world.
type Options struct {
	Subjects int
	// Presence is the chance a patient has facts in a block.
	Presence float64
	// Rare (0 to 1) makes later conditions rarer: small groups.
	Rare float64
}

// Generate is a seeded random world over dims and blocks.
func Generate(seed uint64, d Dims, blocks []calendar.Period, start bool, o Options) World {
	r := rand.New(rand.NewPCG(seed, 0x6174746163)) // #nosec G404 -- synthetic data, not security
	w := World{Dims: d, Blocks: blocks, Start: start}
	k := len(d.Conditions)
	for n := 0; n < o.Subjects; n++ {
		s := Subject{Age: r.IntN(len(d.Ages)), Sex: r.IntN(len(d.Sexes)), Loc: r.IntN(len(d.Locations)),
			Spec: r.IntN(len(d.Specialties)), Source: r.IntN(len(d.Sources)), Blocks: map[int]*Block{}}
		var seen uint32
		if !start {
			for c := 0; c < k; c++ {
				if r.IntN(3) == 0 {
					seen |= 1 << c
				}
			}
		}
		for b := range blocks {
			if r.Float64() >= o.Presence {
				continue
			}
			blk := &Block{Results: map[int]int{}}
			for c := 0; c < k; c++ {
				if r.Float64() < 0.6/(1+o.Rare*float64(c*c)) {
					blk.Conds |= 1 << c
				}
			}
			blk.New = blk.Conds &^ seen
			seen |= blk.Conds
			for f := range d.Findings {
				if r.IntN(3) == 0 {
					blk.Finds |= 1 << f
					blk.Results[f] = 1 + r.IntN(2)
				}
			}
			for c := 0; c < k; c++ {
				if blk.Conds&(1<<c) != 0 {
					for i := r.IntN(3); i > 0; i-- {
						blk.Consults = append(blk.Consults, Consult{c, r.IntN(4), r.IntN(3), r.IntN(4)})
					}
				}
			}
			for i := r.IntN(3); i > 0; i-- {
				ack := -1
				if r.IntN(3) > 0 {
					ack = r.IntN(120)
				}
				blk.Alerts = append(blk.Alerts, Alert{r.IntN(3), r.IntN(5), ack})
			}
			if blk.Conds == 0 && blk.Finds == 0 && len(blk.Alerts) == 0 {
				blk.Alerts = append(blk.Alerts, Alert{r.IntN(3), 0, -1})
			}
			s.Blocks[b] = blk
		}
		if len(s.Blocks) > 0 {
			w.Subjects = append(w.Subjects, s)
		}
	}
	return w
}

// Setting is a subject's setting: its location's.
func (w *World) Setting(s *Subject) string { return w.Dims.Locations[s.Loc].Setting }

// Describe is a short summary, for a failure message.
func (w *World) Describe() string {
	return fmt.Sprintf("%d patients over %d blocks, %d conditions", len(w.Subjects), len(w.Blocks), len(w.Dims.Conditions))
}

func setBits(m uint32) []int {
	var out []int
	for i := 0; m != 0; i++ {
		if m&1 != 0 {
			out = append(out, i)
		}
		m >>= 1
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
