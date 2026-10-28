//go:build !test

package server

import "github.com/Measured-Health/mhai-multiagent/services/core/internal/authz"

// registerFixtures adds nothing in a normal build: the /_test/ routes exist
// only under the test build tag.
func registerFixtures(*authz.Mux, Deps) {}

// FixturesBuilt reports whether the /_test/ fixture routes are compiled in.
const FixturesBuilt = false
