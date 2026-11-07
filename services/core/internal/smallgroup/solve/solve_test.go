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
