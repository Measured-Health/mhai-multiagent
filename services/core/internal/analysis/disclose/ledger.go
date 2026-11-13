package disclose

// Ledger is one period's record (ruling C10): every body published for the
// period and inside it (its blocks and months), and what they publish
// together, as the final check's knowledge. The guard of one context
// costs about a second when that knowledge is rebuilt from the bodies
// (design D6), so the orchestrator keeps a Ledger per period and adds each
// body as it is published, instead of rebuilding it per request (PR 3
// review, condition 3).
//
// A Ledger is not safe for concurrent use: the orchestrator holds the
// period's lock (one guard per hospital and period at a time) and a mutex.
type Ledger struct {
	p    *prior
	have map[string]bool
}

// NewLedger is the record of known, built at once.
func NewLedger(known ...Body) *Ledger {
	l := &Ledger{have: map[string]bool{}}
	var ks []*sheetState
	for i := range known {
		k := bodyKey(&known[i])
		if l.have[k] {
			continue
		}
		l.have[k] = true
		ks = append(ks, stateOf(&known[i]))
	}
	l.p = newPrior(ks)
	return l
}

func bodyKey(b *Body) string {
	st := &sheetState{sh: &sheet{ctx: b.Context}}
	return ctxKey(st)
}

// Len is the number of bodies in the record.
func (l *Ledger) Len() int { return len(l.p.known) }

// Has reports whether the record holds a body for b's context.
func (l *Ledger) Has(b Body) bool { return l.have[bodyKey(&b)] }

// Add records a published body: what it shows, its sets, and the
// equations it makes with the bodies already recorded (its blocks and
// months, its sub-contexts, its locations). A body already recorded for
// the same context is ignored. The result is the knowledge NewLedger
// would build, plus the earlier versions of the partition equations
// (true equations, so only ever more cautious).
func (l *Ledger) Add(b Body) {
	k := bodyKey(&b)
	if l.have[k] {
		return
	}
	l.have[k] = true
	p := l.p
	st := stateOf(&b)
	p.sets = append(p.sets, p.sys.learn(p.before, st)...)
	all := append(append([]*sheetState{}, p.known...), st)
	n := len(all) - 1
	for _, e := range append(append(p.sys.crossSets(all, n), p.sys.partitionSets(all, n)...), p.sys.settingSets(all, n)...) {
		p.before.AddValued(e.vars, e.coefs, 0)
		p.sets = append(p.sets, e)
	}
	p.known = all
	zeroParts(p.before, p.sets)
	// The whole-set rule compares against everything published: every
	// set's shape is taken again on the new record.
	p.shapes = p.shapes[:0]
	for _, e := range p.sets {
		p.shapes = append(p.shapes, p.before.Shape(e.hidden))
	}
}

// Guard is the whole guard of one context (D3, D5; ruling C10) against the
// record: its filter parents among the recorded bodies (FamilyForced: hidden
// whole if its own group is hidden in one), every recorded body known, and
// ParentForced with its children, repeated until nothing changes. The
// children are the period's, with the same filters; any not yet recorded
// is recorded first. The body is not recorded: the caller adds it once it
// is stored (never one that wasn't published).
func (l *Ledger) Guard(raw Raw, children []ChildBodies) (Body, error) {
	if err := CheckFilters(raw.Context.Filters); err != nil {
		return HiddenWhole(raw.Context), err
	}
	for _, c := range children {
		l.Add(c.Body)
	}
	var parents []ParentContext
	for _, st := range l.p.known {
		c := st.sh.ctx
		if c.Period.Equal(raw.Context.Period) && subsetOf(c.Filters, raw.Context.Filters) {
			parents = append(parents, ParentContext{Body: st.body})
		}
	}
	forced, whole := FamilyForced(raw, parents)
	if whole {
		return HiddenWhole(raw.Context), nil
	}
	forced = forced.Union(ParentForced(raw, nil, children))
	for {
		b, err := l.guardResponse(raw, forced)
		if err != nil || b.Whole {
			return b, err
		}
		more := ParentForced(raw, &b, children)
		n := len(forced)
		forced = forced.Union(more)
		if len(forced) == n {
			return b, nil
		}
	}
}
