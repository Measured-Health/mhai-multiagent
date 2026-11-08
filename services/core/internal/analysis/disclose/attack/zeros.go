package attack

import (
	"sort"
	"strconv"
	"strings"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// zeros is every atom the attacker knows is 0 from docs' shown zeros (and
// prev, if given), with what follows from them, to a fixed point:
//
//   - a fact needs a patient in its block with its attributes and block
//     conditions (and, for a result, its finding); a patient with no
//     condition and no finding in a block needs an alert there;
//   - a block's patient needs, in each month or quarter around it that is
//     published, a patient with the same attributes and at least its
//     conditions and findings; such a patient needs blocks that make up
//     its conditions and findings;
//   - with the start known, an existing case needs a case of its
//     condition in an earlier block.
func (m *Model) zeros(docs []int, prev []bool) []bool {
	z := make([]bool, len(m.atoms))
	copy(z, prev)
	in := map[int]bool{}
	for _, d := range docs {
		in[d] = true
	}
	for _, c := range m.Cells {
		if in[c.Doc] && c.Shown && c.Value == 0 {
			for _, t := range c.box {
				if t.coef > 0 {
					z[t.atom] = true
				}
			}
		}
	}
	contiguous := m.w.Start && len(m.blockA) > 0
	for b := range m.blockA {
		for i := 0; i < b; i++ {
			if _, ok := m.blockA[i]; !ok {
				contiguous = false
			}
		}
	}
	type bas struct {
		b, a int
		S    uint32
	}
	type ba struct{ b, a int }
	type ac struct{ a, c int }
	for {
		changed := false
		set := func(i int) {
			if !z[i] {
				z[i], changed = true, true
			}
		}
		live := map[bas]bool{}
		liveF := map[bas]uint32{} // the findings some live patient has
		alertsEmpty := map[ba]bool{}
		earliest := map[ac]int{}
		for b, ids := range m.blockA {
			for _, id := range ids {
				if z[id] {
					continue
				}
				x := m.atoms[id]
				live[bas{b, x.a, x.S}] = true
				liveF[bas{b, x.a, x.S}] |= x.F
				for _, c := range setBits(x.S) {
					if e, ok := earliest[ac{x.a, c}]; !ok || b < e {
						earliest[ac{x.a, c}] = b
					}
				}
			}
		}
		for b, ids := range m.factA {
			for _, id := range ids {
				x := m.atoms[id]
				if !z[id] && x.kind == kAlert && x.S == 0 {
					alertsEmpty[ba{b, x.a}] = true
				}
			}
		}
		// Facts need their patient.
		for b, ids := range m.factA {
			for _, id := range ids {
				if z[id] {
					continue
				}
				x := m.atoms[id]
				k := bas{b, x.a, x.S}
				if !live[k] || (x.kind == kResult && liveF[k]&(1<<x.d) == 0) {
					set(id)
				}
			}
		}
		// Distinct patients of each period, against the blocks.
		type distKey struct {
			a    int
			S, F uint32
		}
		for p, ids := range m.distA {
			var liveDist []distKey
			for _, id := range ids {
				if !z[id] {
					x := m.atoms[id]
					liveDist = append(liveDist, distKey{x.a, x.S, x.F})
				}
			}
			var liveBlocks []distKey
			bs := m.blocksOf(m.periods[p])
			for _, b := range bs {
				for _, id := range m.blockA[b] {
					if z[id] {
						continue
					}
					x := m.atoms[id]
					liveBlocks = append(liveBlocks, distKey{x.a, x.S, x.F})
					ok := false
					for _, d := range liveDist {
						if d.a == x.a && d.S&x.S == x.S && d.F&x.F == x.F {
							ok = true
							break
						}
					}
					if !ok {
						set(id)
					}
				}
			}
			for _, id := range ids {
				if z[id] {
					continue
				}
				x := m.atoms[id]
				var cover, coverF uint32
				any := false
				for _, lb := range liveBlocks {
					if lb.a == x.a && x.S&lb.S == lb.S && x.F&lb.F == lb.F {
						any = true
						cover |= lb.S
						coverF |= lb.F
					}
				}
				if !any || cover != x.S || coverF != x.F {
					set(id)
				}
			}
		}
		for b, ids := range m.blockA {
			for _, id := range ids {
				if z[id] {
					continue
				}
				x := m.atoms[id]
				// A patient with no condition and no finding had an alert.
				if x.S == 0 && x.F == 0 && !alertsEmpty[ba{b, x.a}] {
					set(id)
				}
				if !contiguous {
					continue
				}
				for _, c := range setBits(x.S &^ x.N) {
					if e, ok := earliest[ac{x.a, c}]; !ok || e >= b {
						set(id)
					}
				}
			}
		}
		if !changed {
			return z
		}
	}
}

// unknowns merges atoms that every box treats alike (the same coefficient
// in each, and, when replaying, known 0 at the same step) into one unknown:
// exact, since an attacker only ever sees their sum.
type unknowns struct {
	n      int
	of     []int // atom → unknown (−1: known 0)
	zeroAt []int // unknown → the step it is learnt 0 at (replay)
}

func merge(z []bool, at []int, boxes [][]term) *unknowns {
	sig := make([][]string, 0)
	_ = sig
	n := 0
	for _, bx := range boxes {
		for _, t := range bx {
			if t.atom+1 > n {
				n = t.atom + 1
			}
		}
	}
	keys := make([]strings.Builder, n)
	for bi, bx := range boxes {
		for _, t := range bx {
			keys[t.atom].WriteString(strconv.Itoa(bi) + "*" + strconv.FormatInt(t.coef, 10) + ",")
		}
	}
	u := &unknowns{of: make([]int, n)}
	ids := map[string]int{}
	for a := range keys {
		if z != nil && z[a] {
			u.of[a] = -1
			continue
		}
		k := keys[a].String()
		if at != nil {
			k += "@" + strconv.Itoa(at[a])
		}
		id, ok := ids[k]
		if !ok {
			id = len(ids)
			ids[k] = id
			t := 1 << 30
			if at != nil {
				t = at[a]
			}
			u.zeroAt = append(u.zeroAt, t)
		}
		u.of[a] = id
	}
	u.n = len(ids)
	return u
}

func (u *unknowns) terms(box []term) []solve.Term {
	m := map[int]int64{}
	for _, t := range box {
		if t.atom < len(u.of) && u.of[t.atom] >= 0 {
			m[u.of[t.atom]] = t.coef // merged atoms share each coefficient
		}
	}
	out := make([]solve.Term, 0, len(m))
	for v, c := range m {
		out = append(out, solve.Term{Var: v, Coef: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Var < out[j].Var })
	return out
}

func (u *unknowns) conv(boxes [][]term) [][]solve.Term {
	out := make([][]solve.Term, len(boxes))
	for i, b := range boxes {
		out[i] = u.terms(b)
	}
	return out
}

// ones is a box with non-negative coefficients as a list of unknowns, each
// repeated by its coefficient.
func (u *unknowns) ones(box []term) []int {
	var out []int
	for _, t := range u.terms(box) {
		for i := int64(0); i < t.Coef; i++ {
			out = append(out, t.Var)
		}
	}
	return out
}
