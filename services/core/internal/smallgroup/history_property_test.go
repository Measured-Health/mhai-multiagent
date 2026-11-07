package smallgroup

import (
	"math/rand"
	"testing"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// A7 at the guard's level (Plan 7c spec §6.4, §11.5): random histories of 1
// to 3 years from each hospital's onboarding, every eligible week, month and
// quarter guarded in calendar order, then solve over every published period
// at once (each block's unknowns apart), with shown zeros, unlisted
// hospitals and pre-onboarding weeks as zero equations. No hidden cell and
// no hidden-member sum of 1 to 4 may be exposed, and the guard never gives
// up. Task 4 repeats it from the routes' JSON with a parser of its own.
//
// Weeks are numbered from the history's first month, four a month (weeks
// are month-aligned); a block is three months, twelve weeks.

// historyCount is the number of A7 histories: 500, fewer under -race (CI's
// go test -race runs 50) or -short.
func historyCount() int {
	switch {
	case testing.Short():
		return 40
	case raceEnabled:
		return 50
	}
	return 500
}

type history struct {
	months, hosp int
	epoch        int         // the first eligible week (mid-month)
	onboard      []int       // each hospital's onboarding week
	deact        []int       // the first week after deactivation, or -1
	qg, rv       [][][]int64 // [hospital][week][category]

	checkFullyHidden bool
}

func randomHistory(r *rand.Rand) *history {
	h := &history{months: 12 + r.Intn(25), hosp: 1 + r.Intn(8)}
	weeks := h.months * 4
	// The release epoch: weeks 1 to 3 of a month in the first third.
	h.epoch = r.Intn(max(1, h.months/3))*4 + 1 + r.Intn(3)
	for i := 0; i < h.hosp; i++ {
		ow := 0
		if r.Intn(3) > 0 {
			ow = r.Intn(weeks)
		}
		dw := -1
		if r.Intn(10) < 3 && ow+1 < weeks {
			dw = ow + 1 + r.Intn(weeks-ow-1)
		}
		h.onboard = append(h.onboard, ow)
		h.deact = append(h.deact, dw)
		qg := make([][]int64, weeks)
		rv := make([][]int64, weeks)
		quiet := r.Intn(4) == 0
		for w := 0; w < weeks; w++ {
			qg[w] = make([]int64, 7)
			rv[w] = make([]int64, 2)
			if r.Intn(5) == 0 {
				quiet = !quiet // runs of zeros
			}
			if w < ow || (dw >= 0 && w >= dw) || quiet {
				continue
			}
			fillQG(r, qg[w])
			var answers int64
			for c := 0; c < 6; c++ {
				answers += qg[w][c]
			}
			if answers > 0 {
				rv[w][CatAgreed] = int64(r.Intn(int(min(answers, 9)) + 1))
				rv[w][CatCorrected] = int64(r.Intn(int(min(answers-rv[w][CatAgreed], 4)) + 1))
			}
		}
		h.qg = append(h.qg, qg)
		h.rv = append(h.rv, rv)
	}
	return h
}

// fillQG draws one hospital-week of QG counts: answers biased to the small
// values, some degraded, and unanswered questions in a few weeks.
func fillQG(r *rand.Rand, x []int64) {
	for _, c := range []int{CatPass, CatRegenerate, CatEscalate} {
		n := count(r)
		d := int64(0)
		if r.Intn(3) == 0 && n > 0 {
			d = int64(r.Intn(int(n) + 1))
		}
		x[c], x[c+1] = n-d, d
	}
	if r.Intn(5) == 0 {
		x[CatUnanswered] = int64(r.Intn(4))
	}
}

func (h *history) listed(w int) []int {
	var out []int
	for i := 0; i < h.hosp; i++ {
		if h.onboard[i] <= w && (h.deact[i] < 0 || w < h.deact[i]) {
			out = append(out, i)
		}
	}
	return out
}

func union(lists ...[]int) []int {
	seen := map[int]bool{}
	var out []int
	for i := 0; i < 64; i++ {
		for _, l := range lists {
			for _, x := range l {
				if x == i && !seen[x] {
					seen[x] = true
					out = append(out, x)
				}
			}
		}
	}
	return out
}

// run guards every eligible period of both families, block by block, in
// calendar order, and returns the blocks.
func (h *history) run(t *testing.T) []*Block {
	t.Helper()
	var out []*Block
	for _, fam := range []*Family{QG, R} {
		src := h.qg
		if fam == R {
			src = h.rv
		}
		for q := 0; q*3 < h.months; q++ {
			b := newBlock(fam, 12, h.hosp)
			first := q * 12
			for i := 0; i < h.hosp; i++ {
				b.Onboard[i] = min(12, max(0, h.onboard[i]-first))
				for s := 0; s < 12 && first+s < h.months*4; s++ {
					copy(b.X[i][s], src[i][first+s])
				}
			}
			var months []int
			var monthLists [][]int
			for mo := 0; mo < 3 && q*3+mo < h.months; mo++ {
				var weeks []int
				var lists [][]int
				for wk := 0; wk < 4; wk++ {
					s := mo*4 + wk
					if first+s < h.epoch {
						continue
					}
					l := h.listed(first + s)
					h.guard(t, b, Pending{Slots: []int{s}, Listed: l})
					weeks = append(weeks, len(b.Published)-1)
					lists = append(lists, l)
				}
				if first+mo*4 < h.epoch {
					continue // the month has a week before the epoch: absent
				}
				l := union(lists...)
				h.guard(t, b, Pending{Slots: seq(mo*4, 4), Listed: l, Children: weeks})
				months = append(months, len(b.Published)-1)
				monthLists = append(monthLists, l)
			}
			if first >= h.epoch && len(months) == 3 {
				h.guard(t, b, Pending{Slots: seq(0, 12), Listed: union(monthLists...), Children: months})
			}
			out = append(out, b)
		}
	}
	return out
}

func (h *history) guard(t *testing.T, b *Block, p Pending) {
	t.Helper()
	if h.checkFullyHidden {
		m := len(b.Family.Measures)
		hid := make([]bool, (len(p.Listed)+1)*m)
		for i := range hid {
			hid[i] = true
		}
		all := append(b.shapes(), snap{p.Slots, p.Listed, p.Children, hid})
		si := len(all) - 1
		if newly := b.newlyExposed(all, si, b.before(all, si)); len(newly) > 0 {
			t.Fatalf("S fully hidden still exposes %d boxes", len(newly))
		}
	}
	publish(t, b, p)
}

// A7 at the guard's level.
func TestHistoryAnchoredAtOnboarding(t *testing.T) {
	r := rand.New(rand.NewSource(20261107))
	n := historyCount()
	exposedCells, hiddenCells, published := 0, 0, 0
	for k := 0; k < n; k++ {
		h := randomHistory(r)
		bs := h.run(t)
		for _, b := range bs {
			published += len(b.Published)
			for _, p := range b.Published {
				for _, x := range p.Hidden {
					if x {
						hiddenCells++
					}
				}
			}
		}
		rep := reportAll(bs)
		if l := rep.leaks(); l != "" {
			t.Fatalf("history %d (%d months, %d hospitals, epoch week %d): %s", k, h.months, h.hosp, h.epoch, l)
		}
		exposedCells += rep.exposedCells
	}
	if hiddenCells == 0 {
		t.Fatal("no cell was ever hidden: the generator tests nothing")
	}
	t.Logf("%d histories, %d snapshots, %d hidden cells; %d hidden cells exposed with a value of 0 or 5 and up (already known before they were hidden)",
		n, published, hiddenCells, exposedCells)
}

// Spec §11.6: combinations of hidden cells that aren't a set (two hidden
// week cells of one month row, not all of them) are only measured. The
// number goes in the PR.
func TestUnionsOfHiddenWeeksMeasured(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	pairs, exposedSmall := 0, 0
	for k := 0; k < historyCount()/5; k++ {
		for _, b := range randomHistory(r).run(t) {
			eqs := eqsOf(b, true)
			var boxes [][]int
			m := len(b.Family.Measures)
			for _, p := range b.Published {
				if len(p.Children) != 4 {
					continue
				}
				for row := 0; row <= len(p.Listed); row++ {
					for k := 0; k < m; k++ {
						var hid [][]int
						for _, ci := range p.Children {
							c := b.Published[ci]
							cr := 0
							if row > 0 {
								cr = indexOf(c.Listed, p.Listed[row-1]) + 1
								if cr == 0 {
									continue
								}
							}
							if c.Hidden[cr*m+k] {
								hid = append(hid, cellVars(b, c, cr*m+k))
							}
						}
						for i := 0; i < len(hid); i++ {
							for j := i + 1; j < len(hid); j++ {
								if len(hid) == 2 {
									continue // that is the set's own sum
								}
								boxes = append(boxes, append(append([]int{}, hid[i]...), hid[j]...))
							}
						}
					}
				}
			}
			ex := solve.Exposed(b.nVars(), eqs, boxes)
			for i, e := range ex {
				pairs++
				if e && small(valueOf(b, boxes[i])) {
					exposedSmall++
				}
			}
		}
	}
	t.Logf("pairs of hidden weeks in a month row that aren't the whole set: %d checked, %d exposed with a value of 1 to 4", pairs, exposedSmall)
}
