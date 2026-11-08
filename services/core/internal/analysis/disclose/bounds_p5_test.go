package disclose

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gonum.org/v1/gonum/mat"
	"gonum.org/v1/gonum/optimize/convex/lp"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analysis/disclose/attack"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup/solve"
)

// bound is the range of a target over the system: min and max of Σ
// coef·x subject to its equations, its inequalities and x ≥ 0, by the
// simplex method (gonum, test-only). A variable in no row is free (≥ 0):
// it adds 0 to the minimum and makes the maximum unbounded. ok is false
// when the LP fails.
func bound(s attack.LinSys, target attack.Row) (lo, hi float64, ok bool) {
	col := map[int]int{}
	use := func(ts []solve.Term) {
		for _, t := range ts {
			if _, ok := col[t.Var]; !ok {
				col[t.Var] = len(col)
			}
		}
	}
	for _, r := range s.Eq {
		use(r.Terms)
	}
	for _, r := range s.Ge {
		use(r.Terms)
	}
	free := false
	for _, t := range target.Terms {
		if _, ok := col[t.Var]; !ok && t.Coef > 0 {
			free = true
		}
	}
	nv := len(col)
	n := nv + len(s.Ge) // a slack per inequality
	rows := len(s.Eq) + len(s.Ge)
	if rows == 0 {
		return 0, math.Inf(1), true
	}
	a := mat.NewDense(rows, n, nil)
	b := make([]float64, rows)
	for i, r := range s.Eq {
		for _, t := range r.Terms {
			a.Set(i, col[t.Var], a.At(i, col[t.Var])+float64(t.Coef))
		}
		b[i] = r.Rhs
	}
	for j, r := range s.Ge {
		i := len(s.Eq) + j
		for _, t := range r.Terms {
			a.Set(i, col[t.Var], a.At(i, col[t.Var])+float64(t.Coef))
		}
		a.Set(i, nv+j, -1)
		b[i] = r.Rhs
	}
	solveFor := func(sign float64) (float64, bool) {
		c := make([]float64, n)
		for _, t := range target.Terms {
			if j, ok := col[t.Var]; ok {
				c[j] = sign * float64(t.Coef)
			}
		}
		v, _, err := lp.Simplex(c, a, b, 1e-9, nil)
		if errors.Is(err, lp.ErrUnbounded) {
			return sign * math.Inf(-1), true
		}
		return sign * v, err == nil
	}
	lo, ok1 := solveFor(1)
	hi, ok2 := math.Inf(1), true
	if !free {
		hi, ok2 = solveFor(-1)
	}
	return lo, hi, ok1 && ok2
}

// P5 (design §5.7, R-A2), reported: per response, and per month with its
// blocks under the distinct-count bounds (largest block ≤ month ≤ Σ
// blocks), the share of hidden cells of 1 or more whose feasible range
// excludes 0 and is narrower than 0 to 4. Each system's first 25 hidden
// cells, or all with ANALYSIS_LONG=1.
func TestP5BoundsReported(t *testing.T) {
	if raceEnabled || testing.Short() {
		t.Skip("a report, run without -race (CI's analysis property step)")
	}
	var narrow, total, failed int
	for _, seed := range seeds(100, 1, 1) {
		w := attack.Generate(seed, smallDims(2), octBlocksOf(), false, attack.Options{Subjects: 50, Presence: 0.35, Rare: 0.8})
		var bodies []Body
		var kids []ChildBodies
		for _, b := range w.Blocks {
			raw := rawOf(&w, b, Filters{}, false)
			body, err := GuardPeriod(raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			bodies, kids = append(bodies, body), append(kids, ChildBodies{raw, body})
		}
		month, err := GuardPeriod(rawOf(&w, oct2026, Filters{}, false), kids)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, month)
		var docs []attack.Doc
		for _, b := range bodies {
			d, err := docOf(b)
			if err != nil {
				t.Fatal(err)
			}
			docs = append(docs, d)
		}
		m, err := attack.New(&w, docs)
		if err != nil {
			t.Fatal(err)
		}
		measure := func(s attack.LinSys) {
			for k, tg := range s.Targets {
				if k >= 25 && !long() {
					break // the first 25 per system, all at full size
				}
				lo, hi, ok := bound(s, tg)
				total++
				switch {
				case !ok:
					failed++
				case lo >= 1-1e-6 && hi-lo < 4-1e-6:
					narrow++
				}
			}
		}
		for i := range bodies {
			measure(m.Bounds([]int{i}, false))
		}
		all := []int{0, 1, 2, 3, 4}
		measure(m.Bounds(all, true))
	}
	if failed > 0 {
		t.Errorf("%d of %d LPs failed", failed, total)
	}
	share := 0.0
	if total > 0 {
		share = 100 * float64(narrow) / float64(total)
	}
	t.Logf("R-A2: %d of %d hidden cells of 1 or more bounded away from 0 within a range narrower than 0–4 (%.2f%%)", narrow, total, share)
}

// N13: the LP library is a test dependency only: no non-test file in core
// imports it.
func TestGonumOnlyInTests(t *testing.T) {
	root := filepath.Join("..", "..", "..") // services/core
	found := 0
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		b, err := os.ReadFile(p) // #nosec G304 -- the repository's own sources
		if err != nil {
			return err
		}
		if strings.Contains(string(b), `"gonum.org/`) {
			found++
			if !strings.HasSuffix(p, "_test.go") {
				t.Errorf("%s imports gonum outside a test", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Error("no test imports gonum: the check proves nothing")
	}
}
