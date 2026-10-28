//go:build !test

package server

import "testing"

// In a normal build the /_test/ fixture routes don't exist: not in the
// table, and 404 when asked for.
func TestFixturesAbsentInNormalBuild(t *testing.T) {
	if FixturesBuilt || hasFixtures() {
		t.Fatal("fixture routes compiled into a normal build")
	}
	for _, p := range []string{"/api/v1/_test/clinical", "/api/v1/_test/platform", "/api/v1/_test/clinical-queries/x"} {
		if got := fixtureStatus(t, p); got != 404 {
			t.Errorf("%s: %d, want 404", p, got)
		}
	}
}
