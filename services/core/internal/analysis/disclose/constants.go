// Package disclose is Analysis's disclosure engine (design §5; ADR 0015):
// every number a response carries is guarded here, from raw counts held in
// memory for one computation, before anything is encoded. It is pure: no
// database, no clock, no logging.
//
//   - Layer A (GuardResponse): every number of one context, all five views
//     together (A1–A9), on smallgroup's rules.
//   - D3 (ParentForced): a period guarded with its frozen children.
//   - D5 (FamilyForced): a filtered context guarded with its stored filter
//     parents; CheckFilters refuses more than FilterCap filters.
//   - History: points copied from stored bodies, never recomputed.
//
// A final check runs on what the context and every related stored body
// publish together (smallgroup.Knowledge): no hidden group newly
// determined, and every set's hidden members keep the whole-set rule
// (R41a-5). The tests' attacker (disclose/attack) checks the published JSON
// exactly, with code of its own.
package disclose

import (
	"time"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// The constants of design §5.1 and the plan's Global Constraints. The rates
// live in internal/analysis/routes/limits.go.
const (
	// Min is the smallest group shown: smallgroup's rule 1.
	Min = smallgroup.Min
	// QuerySetMin: a filtered context with fewer patients is hidden whole (A7, D4).
	QuerySetMin = 10
	// FilterCap is the most filters besides the period (D4).
	FilterCap = 2
	// Grace is how long after a block ends it waits before it freezes (D2).
	Grace = 3 * 24 * time.Hour
	// HistoryMonths: the oldest answerable period starts within this many months.
	HistoryMonths = 24
	// HistoryPoints is the most earlier periods a history carries.
	HistoryPoints = 7
	// DrillMax is the most rows a drill-down list sends.
	DrillMax = 200
	// PatientCap is the distinct patients one person may open per calendar quarter.
	PatientCap = 300
	// PairsMax is the most co-occurring pairs a response lists.
	PairsMax = 20
	// RowItemsMax is the most conditions, and findings, a drill row carries.
	RowItemsMax = 10
	// UnusualFilterSets: more distinct filter sets a day from one person is flagged.
	UnusualFilterSets = 100
	// BodyCap is the most stored bodies per hospital and period.
	BodyCap = 20000
	// BodyMaxBytes is the largest stored body.
	BodyMaxBytes = 4 << 20
	// ColdBody is the budget for computing a body that isn't stored yet.
	ColdBody = 2 * time.Second
	// StatementTimeout bounds every analysis statement.
	StatementTimeout = 5 * time.Second
)
