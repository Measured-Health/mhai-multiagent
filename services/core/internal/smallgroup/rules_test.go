package smallgroup

import (
	"encoding/json"
	"regexp"
	"testing"
)

// smallValue is a raw count of 1 to 4 in encoded JSON.
var smallValue = regexp.MustCompile(`"value":\s*[1-4]\s*[,}]`)

// C19: 0 is shown as 0, 1 to 4 are hidden, 5 and up are shown; a negative
// count fails closed.
func TestCountRule(t *testing.T) {
	for n := int64(-2); n <= 50; n++ {
		g := Count(n)
		wantShown := n == 0 || n >= Min
		if g.Shown() != wantShown || g.Suppressed == wantShown {
			t.Errorf("Count(%d) = %+v, shown %v", n, g, g.Shown())
		}
		if g.Shown() && *g.Value != n {
			t.Errorf("Count(%d) shows %d", n, *g.Value)
		}
		b, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		if smallValue.Match(b) {
			t.Errorf("Count(%d) encodes %s", n, b)
		}
		want := `{"value":null,"suppressed":true}`
		if wantShown {
			want = `{"value":` + itoa(n) + `,"suppressed":false}`
		}
		if string(b) != want {
			t.Errorf("Count(%d) encodes %s, want %s", n, b, want)
		}
	}
	// A hidden value with something in Value still encodes null.
	v := int64(3)
	b, _ := json.Marshal(Guarded{Value: &v, Suppressed: true})
	if string(b) != `{"value":null,"suppressed":true}` {
		t.Errorf("a hidden value encodes %s", b)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// C19: every combination of three parts in 0 to 6: no shown 1 to 4, never
// exactly one hidden part, and with the total shown the hidden parts sum to
// 0 or at least 5 (so total − Σ shown is never 1 to 4).
func TestSetNeverOneHidden(t *testing.T) {
	for a := int64(0); a <= 6; a++ {
		for b := int64(0); b <= 6; b++ {
			for c := int64(0); c <= 6; c++ {
				parts := []int64{a, b, c}
				total := a + b + c
				g := Set(total, parts...)
				if len(g) != 4 {
					t.Fatalf("Set returned %d values", len(g))
				}
				vals := append([]int64{total}, parts...)
				var nHidden int
				var hiddenSum int64
				for i, x := range g {
					if x.Shown() && (small(*x.Value) || *x.Value != vals[i]) {
						t.Errorf("%v: member %d shown as %d", vals, i, *x.Value)
					}
					if !x.Shown() && !x.Suppressed {
						t.Errorf("%v: member %d neither shown nor hidden", vals, i)
					}
					if small(vals[i]) && x.Shown() {
						t.Errorf("%v: a small member %d shown", vals, i)
					}
					if i > 0 && x.Suppressed {
						nHidden++
						hiddenSum += vals[i]
					}
				}
				if nHidden == 1 {
					t.Errorf("%v: exactly one hidden part: %+v", vals, g)
				}
				if g[0].Shown() && small(hiddenSum) {
					t.Errorf("%v: total shown, hidden parts sum to %d", vals, hiddenSum)
				}
				b, _ := json.Marshal(g)
				if smallValue.Match(b) {
					t.Errorf("%v encodes %s", vals, b)
				}
			}
		}
	}
}

// Set hides no more than it must when nothing is small.
func TestSetLeavesLargeSetsShown(t *testing.T) {
	for _, x := range Set(30, 10, 0, 20) {
		if !x.Shown() {
			t.Errorf("Set(30, 10, 0, 20): %+v", x)
		}
	}
	// One small part pulls in the smallest other non-zero part.
	g := Set(33, 3, 0, 30)
	if !g[0].Shown() || !g[1].Suppressed || !g[2].Shown() || !g[3].Suppressed {
		t.Errorf("Set(33, 3, 0, 30): %+v", g)
	}
}

// C19: a share is hidden when its numerator or complement is 1 to 4, or the
// denominator is hidden; nothing to divide by is {null, false}; a shown
// share always has four decimals, so it never reads as a small count.
func TestShareRule(t *testing.T) {
	for den := int64(0); den <= 12; den++ {
		for num := int64(0); num <= den; num++ {
			s := Share(num, den)
			wantShown := den >= Min && !small(num) && !small(den-num)
			if den == 0 {
				if s.Shown() || s.Suppressed {
					t.Errorf("Share(0, 0) = %+v, want nothing to show", s)
				}
				continue
			}
			if s.Shown() != wantShown || s.Suppressed == wantShown {
				t.Errorf("Share(%d, %d) = %+v", num, den, s)
			}
			b, _ := json.Marshal(s)
			if smallValue.Match(b) {
				t.Errorf("Share(%d, %d) encodes %s", num, den, b)
			}
		}
	}
	one, _ := json.Marshal(Share(10, 10))
	if string(one) != `{"value":1.0000,"suppressed":false}` {
		t.Errorf("Share(10, 10) encodes %s", one)
	}
	third, _ := json.Marshal(Share(5, 15))
	if string(third) != `{"value":0.3333,"suppressed":false}` {
		t.Errorf("Share(5, 15) encodes %s", third)
	}
	for _, c := range [][2]int64{{-1, 10}, {11, 10}, {5, -5}} {
		if s := Share(c[0], c[1]); !s.Suppressed {
			t.Errorf("Share(%d, %d) = %+v, want hidden", c[0], c[1], s)
		}
	}
	// ShareOf: a hidden numerator (secondary) hides the share.
	if s := ShareOf(10, 40, hidden, Count(40)); !s.Suppressed {
		t.Errorf("ShareOf with a hidden numerator: %+v", s)
	}
	if s := ShareOf(10, 40, Count(10), hidden); !s.Suppressed {
		t.Errorf("ShareOf with a hidden denominator: %+v", s)
	}
	if s := ShareOf(0, 0, Count(0), Count(0)); s.Shown() || s.Suppressed {
		t.Errorf("ShareOf(0, 0): %+v", s)
	}
	if s := ShareOf(10, 40, Count(10), Count(40)); !s.Shown() || *s.Share != 0.25 {
		t.Errorf("ShareOf(10, 40): %+v", s)
	}
}

// Rule 4: one hospital's hidden value isn't all − Σ shown; a second is
// hidden in that column.
func TestAcrossRowsHidesASecondRow(t *testing.T) {
	rows := [][]int64{{23, 20, 3, 0}, {40, 30, 10, 0}, {50, 40, 10, 0}}
	all := []int64{113, 90, 23, 0}
	a, out := AcrossRows(all, rows)
	if !a[0].Shown() || !a[2].Shown() {
		t.Fatalf("all hidden: %+v", a)
	}
	n := 0
	for _, r := range out {
		if r[2].Suppressed {
			n++
		}
	}
	if n < 2 {
		t.Errorf("regenerate column: %d hidden, want at least 2: %+v", n, out)
	}
	if !out[0][2].Suppressed {
		t.Errorf("the small cell is shown: %+v", out[0])
	}
}

// A table that doesn't add up is hidden whole.
func TestAcrossRowsInconsistentHidesAll(t *testing.T) {
	a, out := AcrossRows([]int64{10, 10}, [][]int64{{10, 9}})
	for _, g := range append(a, out[0]...) {
		if !g.Suppressed {
			t.Errorf("inconsistent table shown: %+v %+v", a, out)
		}
	}
	a, out = AcrossRows([]int64{10, 10}, [][]int64{{10, 10, 0}})
	for _, g := range append(a, out[0]...) {
		if !g.Suppressed {
			t.Errorf("ragged table shown: %+v %+v", a, out)
		}
	}
}
