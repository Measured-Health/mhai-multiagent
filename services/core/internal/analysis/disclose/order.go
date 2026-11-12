package disclose

import (
	"sort"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// A6: shown rows first, by value (largest first, then key); hidden rows
// after, in key order. Pairs: the top PairsMax by that order (A5). The
// series stays in date order.
func sortRows[T any](xs []T, val func(*T) smallgroup.Guarded, key func(*T) string) {
	sort.SliceStable(xs, func(i, j int) bool {
		a, b := val(&xs[i]), val(&xs[j])
		if a.Shown() != b.Shown() {
			return a.Shown()
		}
		if a.Shown() && *a.Value != *b.Value {
			return *a.Value > *b.Value
		}
		return key(&xs[i]) < key(&xs[j])
	})
}

type gd = smallgroup.Guarded

func order(b *Body) {
	sortRows(b.ByCondition, func(r *ConditionRow[gd]) gd { return r.Cases }, func(r *ConditionRow[gd]) string { return r.Condition })
	for _, p := range []*[]Part[gd]{&b.ByAge, &b.BySex, &b.BySetting, &b.ByLocation, &b.BySpecialty} {
		sortRows(*p, func(r *Part[gd]) gd { return r.Patients }, func(r *Part[gd]) string { return r.Key })
	}
	sortRows(b.Pairs, func(r *Pair[gd, smallgroup.GuardedShare]) gd { return r.Patients },
		func(r *Pair[gd, smallgroup.GuardedShare]) string { return r.A + "\x00" + r.B })
	if len(b.Pairs) > PairsMax {
		b.Pairs = b.Pairs[:PairsMax]
	}
	sortRows(b.Findings, func(r *FindingRow[gd]) gd { return r.Patients }, func(r *FindingRow[gd]) string { return r.Name + "\x00" + r.Flag })
	sortRows(b.Alerts.DismissReasons, func(r *ReasonRow[gd]) gd { return r.Count }, func(r *ReasonRow[gd]) string { return r.Reason })
	sortRows(b.Quality, func(r *QualityRow[gd, smallgroup.GuardedShare]) gd { return r.Consultations },
		func(r *QualityRow[gd, smallgroup.GuardedShare]) string { return r.Condition })
}

// Drillable is A8: whether a drill key may be set on a row's count: shown
// and at least Min. rowKey names the row as the body's JSON does: a
// condition id (prevalence), "by_age:<band>" and the other partitions
// ("" the null row), "<a>|<b>" (a pair) or "<name>|<flag>" (a finding).
func Drillable(b Body, view View, rowKey string) bool {
	if b.Whole {
		return false
	}
	ok := func(g gd) bool { return g.Shown() && *g.Value >= Min }
	switch view {
	case Prevalence:
		for _, r := range b.ByCondition {
			if r.Condition == rowKey {
				return ok(r.Cases)
			}
		}
	case Affected:
		for name, rows := range map[string][]Part[gd]{"by_age": b.ByAge, "by_sex": b.BySex, "by_setting": b.BySetting, "by_location": b.ByLocation, "by_specialty": b.BySpecialty} {
			for _, r := range rows {
				if name+":"+r.Key == rowKey {
					return ok(r.Patients)
				}
			}
		}
	case CoOccurring:
		for _, r := range b.Pairs {
			if r.A+"|"+r.B == rowKey {
				return ok(r.Patients)
			}
		}
	case Findings:
		for _, r := range b.Findings {
			if r.Name+"|"+r.Flag == rowKey {
				return ok(r.Patients)
			}
		}
	}
	return false
}
