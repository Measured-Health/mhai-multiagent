package disclose

// D5: hide once, hide everywhere, within a family (design §5.4). A
// request's family is its context and its filter parents (P, F \ {f}),
// computed and stored first (plan departure 9), so their guarded states
// are fixed: a cell of the request that is the same group as a parent's
// hidden cell is hidden, and a request whose own group (its patients) is
// hidden in a parent's breakdown is answered hidden whole (T5). Guard
// passes the parents' bodies to GuardResponse as known, so a group a parent
// shows stays shown and the final check runs on the family together.

// FamilyForced is the request's cells that are a group a stored parent
// hides, and whether the request is hidden whole: its patients are a
// group a parent hides, or a parent is hidden whole (a context inside one
// that is hidden whole is too small, or can't be guarded).
func FamilyForced(raw Raw, parents []ParentContext) (Forced, bool) {
	out := Forced{}
	if len(parents) == 0 {
		return out, false
	}
	sh, _, _, _ := layout(&raw)
	own := sh.cells[sh.pat].group
	for i := range parents {
		p := &parents[i].Body
		if p.Whole {
			return out, true
		}
		hid := hiddenGroups(p)
		if hid[own] {
			return out, true
		}
		for _, c := range sh.cells {
			if !c.virtual && c.group != zeroGroup && hid[c.group] {
				out[c.group] = true
			}
		}
	}
	return out, false
}
