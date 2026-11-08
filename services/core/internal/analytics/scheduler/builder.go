// Package scheduler is the analytics scheduler's side (Plan 7c spec §6, §8.3):
// it freezes each finished period's counts, guards them with
// internal/smallgroup and stores one snapshot per period. Raw counts exist
// here only in memory for one computation; they are never logged, put in
// an error or handed to a route. No route package may import scheduler (a test
// fails if internal/platform does).
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/analytics/calendar"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/audit"
	"github.com/Measured-Health/mhai-multiagent/services/core/internal/smallgroup"
)

// Reach is how far back a period's own days can still be read (the
// functions refuse a period that started more than 118 days ago).
const Reach = 118 * 24 * time.Hour

// FrozenRow is one hospital's frozen counts for one period.
type FrozenRow struct {
	Kind       calendar.Kind
	From       time.Time
	Hospital   uuid.UUID
	QG         [7]int64 // smallgroup.CatPass … CatUnanswered
	R          [2]int64 // agreed, corrected
	Clinicians int64
	Decided    int64
	Median     *float64 // hours, over the corrections decided in the period
}

// Hospital is a hospital's name, status and onboarding.
type Hospital struct {
	ID        uuid.UUID
	Name      string
	Status    string
	CreatedAt time.Time
}

// Stored is a stored snapshot of the block.
type Stored struct {
	Period calendar.Period
	State  string // ok, unavailable
	Body   []byte
}

// Source is the database as the scheduler's role sees it (DB), or a fake in
// tests. Freeze returns ErrNotFreezable when the database refuses the
// period (out of reach, a child not frozen, not eligible).
type Source interface {
	Freeze(ctx context.Context, p calendar.Period) error
	Frozen(ctx context.Context, quarter calendar.Period) ([]FrozenRow, error)
	Hospitals(ctx context.Context) ([]Hospital, error)
	Staff(ctx context.Context) (map[uuid.UUID]int64, error)
	Stored(ctx context.Context, quarter calendar.Period) ([]Stored, error)
	Insert(ctx context.Context, p calendar.Period, state string, body []byte) error
	Epoch(ctx context.Context) (time.Time, error)
}

// ErrNotFreezable is the database's refusal to freeze a period (22023).
var ErrNotFreezable = errors.New("scheduler: the period can't be frozen")

// Builder builds one period's snapshot.
type Builder struct {
	Src Source
	// Guard replaces smallgroup.GuardSnapshotCtx (tests: a slow guard).
	Guard func(context.Context, *smallgroup.Block, smallgroup.Pending) ([]bool, error)
	// Budget bounds one period's guard (0: GuardBudget).
	Budget time.Duration
}

// GuardBudget is one period's guard budget (spec §6.5, T17).
const GuardBudget = 60 * time.Second

// The snapshot states.
const (
	StateOK          = "ok"
	StateUnavailable = "unavailable"
)

// ErrGuardTimeout is a period whose guard ran past its budget: nothing is
// stored, and the scheduler retries it (T17).
var ErrGuardTimeout = errors.New("scheduler: the guard ran past its budget")

// errUnguardable is a period that is stored unavailable: counts that don't
// add up, a child not published, a guard that gave up.
var errUnguardable = errors.New("scheduler: the period can't be guarded")

// Build freezes p, guards it with its block's stored snapshots and stores
// it (ON CONFLICT DO NOTHING). A period that can't be counted or guarded is
// stored unavailable; any other error stores nothing (retried later).
func (b Builder) Build(ctx context.Context, p calendar.Period) error {
	ferr := b.Src.Freeze(ctx, p)
	if ferr != nil && !errors.Is(ferr, ErrNotFreezable) {
		return ferr
	}
	budget := b.Budget
	if budget <= 0 {
		budget = GuardBudget
	}
	gctx, cancel := context.WithTimeout(ctx, budget)
	body, err := b.guard(gctx, p, ferr == nil)
	timedOut := errors.Is(gctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	cancel()
	switch {
	case timedOut && err != nil:
		return ErrGuardTimeout
	case errors.Is(err, errUnguardable):
		return b.Src.Insert(ctx, p, StateUnavailable, nil)
	case err != nil:
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(raw) > analytics.MaxBodyBytes {
		return b.Src.Insert(ctx, p, StateUnavailable, nil)
	}
	return b.Src.Insert(ctx, p, StateOK, raw)
}

// MarkUnavailable stores p unavailable (a period out of reach).
func (b Builder) MarkUnavailable(ctx context.Context, p calendar.Period) error {
	return b.Src.Insert(ctx, p, StateUnavailable, nil)
}

// block is one quarter's state for the guard.
type block struct {
	quarter calendar.Period
	weeks   []calendar.Period // its 12 slots
	hosp    []uuid.UUID       // block index → hospital
	index   map[uuid.UUID]int
	rows    map[string]map[uuid.UUID]FrozenRow // period key → hospital → row
}

func key(p calendar.Period) string { return string(p.Kind) + " " + p.From.Format(time.DateOnly) }

func (k *block) slots(p calendar.Period) []int {
	var out []int
	for i, w := range k.weeks {
		if p.Contains(w) {
			out = append(out, i)
		}
	}
	return out
}

// guard builds p's body; frozen is whether the freeze succeeded (p is
// frozen, perhaps with no hospital at all).
func (b Builder) guard(ctx context.Context, p calendar.Period, frozen bool) (analytics.Body, error) {
	q := calendar.BlockOf(p)
	rows, err := b.Src.Frozen(ctx, q)
	if err != nil {
		return analytics.Body{}, err
	}
	k := &block{quarter: q, index: map[uuid.UUID]int{}, rows: map[string]map[uuid.UUID]FrozenRow{}}
	for _, m := range calendar.Children(q) {
		k.weeks = append(k.weeks, calendar.Children(m)...)
	}
	for _, r := range rows {
		pk := key(calendar.Of(r.Kind, r.From))
		if k.rows[pk] == nil {
			k.rows[pk] = map[uuid.UUID]FrozenRow{}
		}
		k.rows[pk][r.Hospital] = r
		if _, ok := k.index[r.Hospital]; !ok {
			k.index[r.Hospital] = -1
		}
	}
	if k.rows[key(p)] == nil {
		if !frozen {
			return analytics.Body{}, errUnguardable // never frozen: out of reach, or a child missing
		}
		k.rows[key(p)] = map[uuid.UUID]FrozenRow{} // frozen with no hospital listed
	}
	for h := range k.index {
		k.hosp = append(k.hosp, h)
	}
	sort.Slice(k.hosp, func(i, j int) bool { return k.hosp[i].String() < k.hosp[j].String() })
	for i, h := range k.hosp {
		k.index[h] = i
	}
	hospitals, err := b.Src.Hospitals(ctx)
	if err != nil {
		return analytics.Body{}, err
	}
	byID := map[uuid.UUID]Hospital{}
	for _, h := range hospitals {
		byID[h.ID] = h
	}
	staff, err := b.Src.Staff(ctx)
	if err != nil {
		return analytics.Body{}, err
	}
	stored, err := b.Src.Stored(ctx, q)
	if err != nil {
		return analytics.Body{}, err
	}
	guard := b.Guard
	if guard == nil {
		guard = smallgroup.GuardSnapshotCtx
	}
	return k.build(ctx, guard, p, byID, staff, stored)
}

// family is one family's guard input built from the block.
func (k *block) family(f *smallgroup.Family, byID map[uuid.UUID]Hospital, counts func(FrozenRow) []int64) (*smallgroup.Block, error) {
	b := &smallgroup.Block{Family: f, Slots: len(k.weeks), Hospitals: len(k.hosp), Onboard: make([]int, len(k.hosp))}
	b.X = make([][][]int64, len(k.hosp))
	for h, id := range k.hosp {
		b.X[h] = make([][]int64, len(k.weeks))
		first := len(k.weeks)
		for s, w := range k.weeks {
			b.X[h][s] = make([]int64, f.Cats)
			if r, ok := k.rows[key(w)][id]; ok {
				copy(b.X[h][s], counts(r))
				for _, v := range b.X[h][s] {
					if v != 0 && s < first {
						first = s
					}
				}
			}
		}
		// Onboarding (§6.1): every week that ended at or before the hospital
		// was created is known zero. A week with counts is never before it.
		on := len(k.weeks)
		if hosp, ok := byID[id]; ok {
			on = 0
			for on < len(k.weeks) && !k.weeks[on].To.After(hosp.CreatedAt) {
				on++
			}
		}
		b.Onboard[h] = min(on, first)
	}
	return b, nil
}

func qgCounts(r FrozenRow) []int64 { return r.QG[:] }
func rCounts(r FrozenRow) []int64  { return r.R[:] }

// published turns the block's stored snapshots into the guard's view, in
// calendar order: the slots, listed hospitals (in the body's order),
// children and hidden cells of each family.
func (k *block) published(stored []Stored) (map[string]int, []analytics.Body, error) {
	sort.SliceStable(stored, func(i, j int) bool {
		a, b := stored[i].Period, stored[j].Period
		if !a.To.Equal(b.To) {
			return a.To.Before(b.To)
		}
		return kindOrder(a.Kind) < kindOrder(b.Kind)
	})
	idx := map[string]int{}
	var bodies []analytics.Body
	for _, s := range stored {
		if s.State != StateOK {
			continue
		}
		body, err := analytics.DecodeBody(s.Body, s.Period)
		if err != nil {
			return nil, nil, errUnguardable
		}
		idx[key(s.Period)] = len(bodies)
		bodies = append(bodies, body)
	}
	return idx, bodies, nil
}

func kindOrder(k calendar.Kind) int {
	switch k {
	case calendar.Week:
		return 0
	case calendar.Month:
		return 1
	}
	return 2
}

// hiddenQG and hiddenR are a stored row's hidden cells, in the guard's
// measure order.
func hiddenQG(r analytics.Row) []bool {
	return []bool{!r.Questions.Shown(), !r.Answers.Shown(), !r.Pass.Shown(), !r.Regenerate.Shown(), !r.Escalate.Shown(), !r.Degraded.Shown()}
}

func hiddenR(r analytics.Row) []bool {
	// agreed and corrected are known exactly when the rate is shown, or when
	// a shown review count is 0.
	known := r.AgreementRate.Shown() || (r.Reviews.Shown() && *r.Reviews.Value == 0)
	return []bool{!r.Reviews.Shown(), !known, !known}
}

func (k *block) build(ctx context.Context, guard func(context.Context, *smallgroup.Block, smallgroup.Pending) ([]bool, error),
	p calendar.Period, byID map[uuid.UUID]Hospital, staff map[uuid.UUID]int64, stored []Stored) (analytics.Body, error) {
	idx, bodies, err := k.published(stored)
	if err != nil {
		return analytics.Body{}, err
	}
	children := calendar.Children(p)
	for _, c := range children {
		if _, ok := idx[key(c)]; !ok {
			return analytics.Body{}, errUnguardable // a child unavailable: so is the parent
		}
	}
	// S's rows, in the body's order: by name, then id.
	var listed []uuid.UUID
	for h := range k.rows[key(p)] {
		listed = append(listed, h)
	}
	sort.Slice(listed, func(i, j int) bool {
		a, b := byID[listed[i]].Name, byID[listed[j]].Name
		if a != b {
			return a < b
		}
		return listed[i].String() < listed[j].String()
	})
	// The linear counts of a month or quarter are its children's sums, or
	// the guard's model is wrong: fail closed.
	if len(children) > 0 {
		for _, h := range listed {
			var qg [7]int64
			var r [2]int64
			for _, c := range children {
				cr := k.rows[key(c)][h]
				for i := range qg {
					qg[i] += cr.QG[i]
				}
				r[0] += cr.R[0]
				r[1] += cr.R[1]
			}
			own := k.rows[key(p)][h]
			if own.QG != qg || own.R != r {
				return analytics.Body{}, errUnguardable
			}
		}
	}
	// Every stored snapshot of the block, in calendar order.
	order := make([]string, len(bodies))
	for kk, i := range idx {
		order[i] = kk
	}
	hidden := map[*smallgroup.Family][]bool{}
	for _, f := range []*smallgroup.Family{smallgroup.QG, smallgroup.R} {
		counts := qgCounts
		if f == smallgroup.R {
			counts = rCounts
		}
		gb, err := k.family(f, byID, counts)
		if err != nil {
			return analytics.Body{}, err
		}
		pubIndex := map[string]int{}
		for _, kk := range order {
			body := bodies[idx[kk]]
			bp := periodOfBody(body)
			var lst []int
			m := len(f.Measures)
			hid := make([]bool, 0, (len(body.Hospitals)+1)*m)
			if f == smallgroup.QG {
				hid = append(hid, hiddenQG(body.All)...)
			} else {
				hid = append(hid, hiddenR(body.All)...)
			}
			for _, hr := range body.Hospitals {
				id, _ := uuid.Parse(hr.Hospital.ID)
				hi, ok := k.index[id]
				if !ok {
					return analytics.Body{}, errUnguardable
				}
				lst = append(lst, hi)
				if f == smallgroup.QG {
					hid = append(hid, hiddenQG(hr.Row)...)
				} else {
					hid = append(hid, hiddenR(hr.Row)...)
				}
			}
			var ch []int
			for _, c := range calendar.Children(bp) {
				if j, ok := pubIndex[key(c)]; ok {
					ch = append(ch, j)
				}
			}
			pubIndex[kk] = len(gb.Published)
			gb.Published = append(gb.Published, smallgroup.Published{Slots: k.slots(bp), Listed: lst, Children: ch, Hidden: hid})
		}
		var ch []int
		for _, c := range children {
			ch = append(ch, pubIndex[key(c)])
		}
		var lst []int
		for _, h := range listed {
			lst = append(lst, k.index[h])
		}
		forced := make([]bool, (len(lst)+1)*len(f.Measures))
		for r, h := range listed {
			if !audit.ShowableName(byID[h].Name) {
				for j := 0; j < len(f.Measures); j++ {
					forced[(r+1)*len(f.Measures)+j] = true
				}
			}
		}
		hid, err := guard(ctx, gb, smallgroup.Pending{Slots: k.slots(p), Listed: lst, Children: ch, Forced: forced})
		if ctx.Err() != nil {
			return analytics.Body{}, ctx.Err()
		}
		if err != nil {
			return analytics.Body{}, errUnguardable
		}
		hidden[f] = hid
	}
	return render(p, listed, k.rows[key(p)], byID, staff, hidden[smallgroup.QG], hidden[smallgroup.R]), nil
}

func periodOfBody(b analytics.Body) calendar.Period {
	from, _ := time.Parse(time.DateOnly, b.From)
	return calendar.Of(calendar.Kind(b.Kind), from)
}

// render is the guarded body of p from its frozen rows and hidden cells.
func render(p calendar.Period, listed []uuid.UUID, rows map[uuid.UUID]FrozenRow, byID map[uuid.UUID]Hospital, staff map[uuid.UUID]int64,
	hidQG, hidR []bool) analytics.Body {
	from, to := analytics.Dates(p)
	body := analytics.Body{Kind: string(p.Kind), From: from, To: to, Hospitals: make([]analytics.HospitalRow, 0, len(listed))}
	var all FrozenRow
	clin := make([]int64, len(listed))
	clinForced := make([]bool, len(listed)+1)
	for i, h := range listed {
		clinForced[i+1] = !audit.ShowableName(byID[h].Name)
		r := rows[h]
		for c := range all.QG {
			all.QG[c] += r.QG[c]
		}
		all.R[0] += r.R[0]
		all.R[1] += r.R[1]
		clin[i] = r.Clinicians
		all.Clinicians += r.Clinicians
	}
	// Active clinicians: a set across hospitals (each user belongs to one).
	cg := smallgroup.SetForced(all.Clinicians, clin, clinForced)
	qgM, rM := len(smallgroup.QG.Measures), len(smallgroup.R.Measures)
	row := func(r FrozenRow, i int, named bool) analytics.Row {
		qh := hidQG[i*qgM : (i+1)*qgM]
		rh := hidR[i*rM : (i+1)*rM]
		q := r.QG
		vals := []int64{
			q[0] + q[1] + q[2] + q[3] + q[4] + q[5] + q[6],
			q[0] + q[1] + q[2] + q[3] + q[4] + q[5],
			q[0] + q[1], q[2] + q[3], q[4] + q[5], q[1] + q[3] + q[5],
		}
		g := make([]smallgroup.Guarded, qgM)
		for k := range g {
			g[k] = guarded(vals[k], qh[k])
		}
		reviews := guarded(r.R[0]+r.R[1], rh[smallgroup.RReviews])
		agreed := guarded(r.R[0], rh[smallgroup.RAgreed])
		out := analytics.Row{
			Questions: g[0], Answers: g[1], Pass: g[2], Regenerate: g[3], Escalate: g[4], Degraded: g[5],
			PassShare:       smallgroup.ShareOf(vals[2], vals[1], g[2], g[1]),
			RegenerateShare: smallgroup.ShareOf(vals[3], vals[1], g[3], g[1]),
			EscalateShare:   smallgroup.ShareOf(vals[4], vals[1], g[4], g[1]),
			Reviews:         reviews,
			AgreementRate:   smallgroup.ShareOf(r.R[0], r.R[0]+r.R[1], agreed, reviews),
		}
		out.ActiveClinicians = cg[i]
		switch {
		case !out.Questions.Shown() || !out.ActiveClinicians.Shown():
			out.QuestionsPerClinician = analytics.Ratio{Number: analytics.Number{Suppressed: true}}
		case *out.ActiveClinicians.Value == 0:
			out.QuestionsPerClinician = analytics.Ratio{}
		default:
			out.QuestionsPerClinician = analytics.RatioOf(float64(*out.Questions.Value) / float64(*out.ActiveClinicians.Value))
		}
		switch {
		case i == 0:
			out.MedianReviewHours = analytics.Hours{} // not additive: the routes take medians over hospitals
		case !named || r.Decided < analytics.MinReviewsForMedian || r.Median == nil:
			// Review fix 3: one representation whether 0 or 1 to 4 were
			// decided, so the hiding says nothing about which.
			out.MedianReviewHours = analytics.Hours{Number: analytics.Number{Suppressed: true}}
		default:
			out.MedianReviewHours = analytics.HoursOf(*r.Median)
		}
		return out
	}
	body.All = row(all, 0, true)
	for i, h := range listed {
		hosp := byID[h]
		named := audit.ShowableName(hosp.Name)
		hr := analytics.HospitalRow{Hospital: analytics.Ref{ID: h.String(), Name: hosp.Name}, Staff: analytics.StaffFigure(staff[h]), Row: row(rows[h], i+1, named)}
		if !named {
			hr.Hospital.Name, hr.Staff = "", smallgroup.Guarded{Suppressed: true}
		}
		body.Hospitals = append(body.Hospitals, hr)
	}
	return body
}

func guarded(v int64, hidden bool) smallgroup.Guarded {
	if hidden {
		return smallgroup.Guarded{Suppressed: true}
	}
	g := smallgroup.Count(v)
	if !g.Shown() {
		return smallgroup.Guarded{Suppressed: true}
	}
	return g
}
