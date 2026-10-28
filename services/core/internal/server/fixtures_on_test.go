//go:build test

package server

import "testing"

// Under the test tag the fixtures are registered (behind the guard, so a
// table-only mux answers 503, not 404).
func TestFixturesPresentUnderTestTag(t *testing.T) {
	if !FixturesBuilt || !hasFixtures() {
		t.Fatal("fixture routes missing under the test tag")
	}
	if got := fixtureStatus(t, "/api/v1/_test/clinical"); got == 404 {
		t.Fatal("fixture route not served")
	}
}
