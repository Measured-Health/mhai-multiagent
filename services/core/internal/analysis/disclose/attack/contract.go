package attack

import (
	"encoding/json"
	"fmt"
	"math"
)

// ContractViolations checks the published JSON against the contract's
// rules that aren't equations, by the attacker's own reading:
//
//   - A4: a share is shown only beside its shown numerator and
//     denominator, is their rounded quotient, and leaves neither 1 to 4;
//     a median is shown only over a shown count, a whole number or a half;
//   - A6: in each list, shown rows come first by value (largest first,
//     then key), hidden rows after, in key order;
//   - the pairs' top 20: at most 20 pairs;
//   - A7: a filtered view shows no patients count under 10, and a view
//     whose patients are hidden shows nothing and lists nothing.
func (m *Model) ContractViolations() []string {
	var out []string
	bad := func(di int, f string, a ...any) {
		out = append(out, fmt.Sprintf("doc %d: "+f, append([]any{di}, a...)...))
	}
	truth := map[string]int64{}
	for _, c := range m.Cells {
		truth[fmt.Sprint(c.Doc, c.Name)] = c.Truth
	}
	for di, doc := range m.Docs {
		views := map[string]obj{}
		for v, j := range doc.Views {
			var o obj
			if json.Unmarshal(j, &o) != nil {
				bad(di, "%s isn't JSON", v)
				continue
			}
			views[v] = o
		}
		pv := views["prevalence"]
		pShown, pVal, _ := guarded(pv["patients"])
		if len(doc.Filters) > 0 && pShown && pVal < 10 {
			bad(di, "A7: a filtered view shows %d patients", pVal)
		}
		lists := []struct {
			view, field, val string
			key              func(obj) string
		}{
			{"prevalence", "by_condition", "cases", func(r obj) string { return id(r["condition"]) }},
			{"affected", "by_age", "patients", func(r obj) string { return strOr(r["band"]) }},
			{"affected", "by_sex", "patients", func(r obj) string { return strOr(r["sex"]) }},
			{"affected", "by_setting", "patients", func(r obj) string { return strOr(r["setting"]) }},
			{"affected", "by_location", "patients", func(r obj) string { return id(r["location"]) }},
			{"affected", "by_specialty", "patients", func(r obj) string { return strOr(r["specialty"]) }},
			{"co-occurring", "pairs", "patients", func(r obj) string { return id(r["a"]) + "\x00" + id(r["b"]) }},
			{"findings", "findings", "patients", func(r obj) string { return strOr(r["name"]) + "\x00" + strOr(r["flag"]) }},
			{"quality", "by_condition", "consultations", func(r obj) string { return id(r["condition"]) }},
		}
		for _, l := range lists {
			rows := list(views[l.view][l.field])
			if !pShown && len(rows) > 0 {
				bad(di, "A7: %s.%s lists rows beside hidden patients", l.view, l.field)
			}
			if l.field == "pairs" && len(rows) > 20 {
				bad(di, "%d pairs, more than 20", len(rows))
			}
			if !ordered(rows, l.val, l.key) {
				bad(di, "A6: %s.%s out of order", l.view, l.field)
			}
		}
		al, _ := views["findings"]["alerts"].(obj)
		if rs := list(al["dismiss_reasons"]); !ordered(rs, "count", func(r obj) string { return strOr(r["reason"]) }) || (!pShown && len(rs) > 0) {
			bad(di, "A6/A7: dismiss_reasons")
		}
		if !pShown {
			tot, _ := pv["total"].(obj)
			for _, v := range []any{tot["cases"], tot["new"], tot["existing"], al["total"], al["dismissed"]} {
				if s, _, _ := guarded(v); s {
					bad(di, "A7: a number shown beside hidden patients")
				}
			}
		}
		// A4: shares.
		for _, r := range list(views["co-occurring"]["pairs"]) {
			ro, _ := r.(obj)
			aS, a, _ := guarded(ro["a_patients"])
			bS, b, _ := guarded(ro["patients"])
			share(di, "share_of_a", ro["share_of_a"], aS && bS, b, a, bad)
		}
		for _, r := range list(views["quality"]["by_condition"]) {
			ro, _ := r.(obj)
			rS, rated, _ := guarded(ro["rated"])
			agreed := truth[fmt.Sprint(di, "quality "+id(ro["condition"])+".agreed")]
			share(di, "agreement_rate", ro["agreement_rate"], rS, agreed, rated, bad)
		}
		// A4: medians.
		tS, _, _ := guarded(al["total"])
		sev, _ := al["by_severity"].(obj)
		cS, _, _ := guarded(sev["critical"])
		median(di, "median_ack_minutes", al["median_ack_minutes"], tS, bad)
		median(di, "critical_median_ack_minutes", al["critical_median_ack_minutes"], tS && cS, bad)
	}
	return out
}

func id(v any) string {
	o, _ := v.(obj)
	return strOr(o["id"])
}

// ordered is A6 for one list.
func ordered(rows []any, val string, key func(obj) string) bool {
	type row struct {
		shown bool
		v     int64
		k     string
	}
	var rs []row
	for _, r := range rows {
		ro, _ := r.(obj)
		s, v, _ := guarded(ro[val])
		rs = append(rs, row{s, v, key(ro)})
	}
	for i := 1; i < len(rs); i++ {
		a, b := rs[i-1], rs[i]
		switch {
		case a.shown && b.shown:
			if a.v < b.v || (a.v == b.v && a.k > b.k) {
				return false
			}
		case !a.shown && b.shown:
			return false
		case !a.shown && !b.shown:
			if a.k > b.k {
				return false
			}
		}
	}
	return true
}

func small(n int64) bool { return n >= 1 && n <= 4 }

// share is A4 for a share of num out of den: shown only when allowed (its
// counts shown), the rounded quotient, leaving neither part 1 to 4.
func share(di int, name string, v any, allowed bool, num, den int64, bad func(int, string, ...any)) {
	o, _ := v.(obj)
	sup, _ := o["suppressed"].(bool)
	x, isNum := o["value"].(float64)
	if sup || !isNum {
		return
	}
	switch {
	case !allowed:
		bad(di, "A4: %s shown beside a hidden count", name)
	case den <= 0 || small(num) || small(den-num):
		bad(di, "A4: %s shown though it gives 1 to 4 away (%d of %d)", name, num, den)
	case math.Abs(x-math.Round(float64(num)/float64(den)*1e4)/1e4) > 1e-9:
		bad(di, "A4: %s is %v, not %d of %d", name, x, num, den)
	}
}

// median is A4 for a median: shown only over a shown count, a whole number
// of minutes or a half.
func median(di int, name string, v any, allowed bool, bad func(int, string, ...any)) {
	o, _ := v.(obj)
	sup, _ := o["suppressed"].(bool)
	x, isNum := o["value"].(float64)
	if sup || !isNum {
		return
	}
	if !allowed || x < 0 || x*2 != math.Trunc(x*2) {
		bad(di, "A4: %s = %v", name, x)
	}
}

// GroupExposure counts hidden groups (distinct boxes, each counted once
// however many cells and docs publish it) of docs holding 1 or more that
// all of docs together determine: small (1 to 4) and secondary (5 or
// more), each hidden and exposed.
func (m *Model) GroupExposure(docs []int) (small, smallExposed, secondary, secondaryExposed int) {
	exposed := map[string]bool{}
	for _, i := range m.Exposed(docs) {
		exposed[boxKey(m.Cells[i].box)] = true
	}
	seen := map[string]bool{}
	for _, i := range m.HiddenProtected(docs) {
		c := m.Cells[i]
		k := boxKey(c.box)
		if seen[k] {
			continue
		}
		seen[k] = true
		if c.Truth <= 4 {
			small++
			if exposed[k] {
				smallExposed++
			}
		} else {
			secondary++
			if exposed[k] {
				secondaryExposed++
			}
		}
	}
	return
}
