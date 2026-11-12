package solve

import "testing"

func TestExposed(t *testing.T) {
	// x0 + x1 = a, x1 + x2 = b, x2 = c: everything is exposed.
	got := Exposed(4, [][]int{{0, 1}, {1, 2}, {2}}, [][]int{{0}, {1}, {0, 2}, {3}, {}})
	want := []bool{true, true, true, false, true}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("box %d: %v, want %v", i, got[i], want[i])
		}
	}
	// A cycle gives no single unknown, only the sums.
	got = Exposed(4, [][]int{{0, 1}, {2, 3}, {0, 2}, {1, 3}}, [][]int{{0}, {0, 1, 2, 3}, {0, 3}})
	if got[0] || !got[1] || got[2] {
		t.Errorf("a cycle: %v", got)
	}
	// Coefficients: 2x = 2 gives x; x − y with x + y gives both.
	g := ExposedLinear(2, [][]Term{{{0, 1}, {1, -1}}, {{0, 1}, {1, 1}}}, [][]Term{{{1, 1}}})
	if !g[0] {
		t.Error("x − y and x + y don't give y")
	}
	// Separate components: a box across two is exposed only if both parts are.
	got = Exposed(4, [][]int{{0}, {2, 3}}, [][]int{{0, 2}, {0, 2, 3}})
	if got[0] || !got[1] {
		t.Errorf("components: %v", got)
	}
}

// The exact whole-set check (ruling R41a-5), moved here from 7c's
// historytest so Analysis's checker uses the same code (plan Task 5).
func TestBasisShapeAndViolates(t *testing.T) {
	b := NewBasis()
	// Nothing known: nothing about {x0, x1} is determined.
	if d, s := b.Shape([][]int{{0}, {1}}); d != 0 || s {
		t.Fatalf("empty: dim %d sum %v", d, s)
	}
	if !b.Add([]int{0, 1}) || b.Add([]int{1, 0}) {
		t.Fatal("Add: the second copy of x0 + x1 isn't independent")
	}
	// x0 + x1 known: the full sum alone.
	if d, s := b.Shape([][]int{{0}, {1}}); d != 1 || !s {
		t.Fatalf("sum: dim %d sum %v", d, s)
	}
	// A coefficient row: x2 − x3 known gives no single member, no sum.
	b.AddTerms([]Term{{2, 1}, {3, -1}})
	if d, s := b.Shape([][]int{{2}, {3}}); d != 1 || s {
		t.Fatalf("difference: dim %d sum %v", d, s)
	}
	b.Add([]int{0})
	if d, _ := b.Shape([][]int{{0}, {1}}); d != 2 {
		t.Fatalf("both known: dim %d", d)
	}
	for _, c := range []struct {
		dimPre       int
		dimFin       int
		sumFin, want bool
	}{
		{0, 0, false, false}, // nothing, before and after
		{0, 1, true, false},  // nothing → the full sum alone
		{0, 1, false, true},  // nothing → a part
		{0, 2, true, true},   // nothing → everything
		{1, 1, true, false},  // unchanged
		{1, 2, true, true},   // the sum known, then a member
	} {
		if got := Violates(c.dimPre, false, c.dimFin, c.sumFin); got != c.want {
			t.Errorf("Violates(%d, %d, %v) = %v, want %v", c.dimPre, c.dimFin, c.sumFin, got, c.want)
		}
	}
}
